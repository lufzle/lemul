# The two-role split: runner vs workspace task.
#
# This file is the load-bearing security property of the whole deployment, so it
# is worth being explicit about what must never be true:
#
#   the runner role must never gain bedrock:*  -- it would let the control-only
#                                                 half reach the model
#   the task role must never gain ecs:*        -- it would let a sandbox place
#                                                 or stop tasks
#
# Neither escalates into the other. A reviewer should be able to read exactly
# that off these policies without cross-referencing anything.

# --- Runner: places tasks, and nothing else --------------------------------

resource "aws_iam_role" "runner" {
  name               = "${var.name_prefix}-runner"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  tags               = var.tags
}

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "runner" {
  # RunTask is scoped to ONE task definition family. The wildcard is on the
  # revision only (":*"), which is what lets a new image roll out without
  # touching this policy -- the whole reason the driver passes a family rather
  # than a pinned revision.
  statement {
    sid       = "PlaceWorkspaceTasks"
    actions   = ["ecs:RunTask"]
    resources = ["arn:${data.aws_partition.current.partition}:ecs:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:task-definition/${aws_ecs_task_definition.workspace.family}:*"]
    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = [aws_ecs_cluster.this.arn]
    }
  }

  # StopTask cannot be scoped to a task definition -- task ARNs are generated at
  # launch, so there is nothing to name in advance. The cluster condition is the
  # available bound, and it is a real one: the runner cannot stop tasks anywhere
  # else in the account.
  statement {
    sid       = "StopWorkspaceTasks"
    actions   = ["ecs:StopTask", "ecs:DescribeTasks", "ecs:ListTasks"]
    resources = ["*"]
    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = [aws_ecs_cluster.this.arn]
    }
  }

  # RunTask with a task role requires explicit permission to pass it. Scoped to
  # exactly the three roles the workspace task names, so the runner cannot hand
  # a workspace some other, more privileged identity.
  statement {
    sid       = "PassOnlyTheWorkspaceRoles"
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.workspace_task.arn, aws_iam_role.execution.arn]
    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ecs-tasks.amazonaws.com"]
    }
  }

  # The third: the role ECS assumes to create and attach the workspace volume.
  # Passed to EC2 rather than to ECS tasks, so it needs its own statement with
  # its own service condition -- the one above would not cover it, and widening
  # that condition instead would let the workspace roles be passed to EC2 too.
  dynamic "statement" {
    for_each = var.workspace_volume_gib > 0 ? [1] : []
    content {
      sid       = "PassTheVolumeInfrastructureRole"
      actions   = ["iam:PassRole"]
      resources = [aws_iam_role.volume_infrastructure[0].arn]
      condition {
        test     = "StringEquals"
        variable = "iam:PassedToService"
        values   = ["ecs.amazonaws.com"]
      }
    }
  }

  # The workspace disk, and the one real widening of the split this file opens
  # with (section 2.1). ECS creates and attaches the volume through the
  # infrastructure role above, but it will not SNAPSHOT one -- and a workspace's
  # filesystem between tasks is a snapshot, because ECS gives a task a new
  # volume or one from a snapshot and never an existing volume.
  #
  # Scoped by the tags ECS puts on the volume for us, which is what keeps this
  # narrow: the runner can snapshot and release volumes IT caused to exist, and
  # nothing else in the account. CreateSnapshot needs the volume condition on
  # the volume and a separate unconditioned statement for the snapshot it mints,
  # because the snapshot does not exist yet to carry a tag.
  dynamic "statement" {
    for_each = var.workspace_volume_gib > 0 ? [1] : []
    content {
      sid       = "PreserveWorkspaceDisks"
      actions   = ["ec2:CreateSnapshot", "ec2:DeleteVolume"]
      resources = ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:volume/*"]
      # StringLike, not StringEquals. StringEquals with "*" matches a tag whose
      # literal value is an asterisk and nothing else -- the condition would
      # never be satisfied, and every snapshot would fail with an access denial
      # naming a tag the volume plainly has.
      condition {
        test     = "StringLike"
        variable = "aws:ResourceTag/lemul:workspace"
        values   = ["*"]
      }
    }
  }

  dynamic "statement" {
    for_each = var.workspace_volume_gib > 0 ? [1] : []
    content {
      sid = "MintAndCollectWorkspaceSnapshots"
      actions = [
        "ec2:CreateSnapshot",
        "ec2:CreateTags",
        "ec2:DeleteSnapshot",
        "ec2:DescribeSnapshots",
      ]
      resources = ["*"]
    }
  }

  # Tagging is on by default for RunTask with tags; without this the call fails
  # rather than placing an untagged task, which would break per-workspace cost
  # attribution silently.
  statement {
    sid       = "TagTasks"
    actions   = ["ecs:TagResource"]
    resources = ["*"]
    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = [aws_ecs_cluster.this.arn]
    }
  }
}

resource "aws_iam_role_policy" "runner" {
  name   = "${var.name_prefix}-runner"
  role   = aws_iam_role.runner.id
  policy = data.aws_iam_policy_document.runner.json
}

# The runner reads its own tunnel token from Secrets Manager. Delivered as a
# secret rather than an environment variable so the value is not visible in
# DescribeTaskDefinition, which is readable by far more principals than the
# secret is.
data "aws_iam_policy_document" "runner_secret" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.runner_token.arn]
  }
}

resource "aws_iam_role_policy" "runner_secret" {
  name   = "${var.name_prefix}-runner-secret"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.runner_secret.json
}

# --- Volume infrastructure role: what ECS uses to attach the disk -----------
#
# A THIRD principal, and it is ECS's rather than ours: ECS assumes it to create,
# attach, tag and detach the workspace's EBS volume. It is required whenever a
# volume is configured at launch -- there is no default -- and the managed
# policy is AWS's own, so this deliberately does not hand-roll an equivalent.
#
# It does not weaken section 2.1's split. Nothing we run assumes this role: the
# runner may only PASS it, to ECS, for exactly this purpose.

resource "aws_iam_role" "volume_infrastructure" {
  count              = var.workspace_volume_gib > 0 ? 1 : 0
  name               = "${var.name_prefix}-volume-infrastructure"
  assume_role_policy = data.aws_iam_policy_document.ecs_service_assume.json
  tags               = var.tags
}

data "aws_iam_policy_document" "ecs_service_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type = "Service"
      # ecs.amazonaws.com, not ecs-tasks: this is the ECS control plane acting
      # on our behalf, not something running inside a task.
      identifiers = ["ecs.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy_attachment" "volume_infrastructure" {
  count      = var.workspace_volume_gib > 0 ? 1 : 0
  role       = aws_iam_role.volume_infrastructure[0].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonECSInfrastructureRolePolicyForVolumes"
}

# --- Workspace task: reaches the model, and nothing else -------------------

resource "aws_iam_role" "workspace_task" {
  name               = "${var.name_prefix}-workspace-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  tags               = var.tags
}

# Bedrock access is OPTIONAL and off by default.
#
# Decision #12 made a customer-hosted gateway the supported inference path, so
# the default workspace task needs no AWS model permissions at all -- it talks to
# the gateway over HTTPS. This attaches only for a customer running
# direct-to-Bedrock, which section 12.5 keeps as a draft.
data "aws_iam_policy_document" "workspace_bedrock" {
  count = var.enable_bedrock ? 1 : 0

  statement {
    sid = "InvokePinnedModels"
    actions = [
      "bedrock:InvokeModel",
      "bedrock:InvokeModelWithResponseStream",
    ]
    resources = var.bedrock_model_arns
  }

  # The preflight's layer 1: reading entitlement is not invoking, and it is what
  # turns "the session died on its first prompt" into a diagnosable verdict
  # (section 12.3).
  statement {
    sid = "ReadModelEntitlement"
    actions = [
      "bedrock:ListInferenceProfiles",
      "bedrock:GetInferenceProfile",
      "bedrock:GetFoundationModelAvailability",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "workspace_bedrock" {
  count  = var.enable_bedrock ? 1 : 0
  name   = "${var.name_prefix}-workspace-bedrock"
  role   = aws_iam_role.workspace_task.id
  policy = data.aws_iam_policy_document.workspace_bedrock[0].json
}

# NO S3 SNAPSHOT PERMISSIONS, and their absence is the point.
#
# The workspace task used to hold s3:GetObject/PutObject/DeleteObject on a
# snapshot bucket, for section 9's recorded "snapshot to S3 on suspend". The
# medium changed to EBS snapshots on 2026-08-02, and with it WHO does the
# preserving: the runner snapshots the volume from outside, so the sandbox needs
# no storage permission at all.
#
# That is a narrowing of the task role rather than a move. A session is a shell
# (section 2.5), so any credential the task holds is a credential its members
# hold -- and read/write on a bucket containing every workspace's uncommitted
# state is precisely the kind one member should not be able to reach for.

# --- Execution role: pulls the image and writes logs -----------------------
#
# Distinct from the task role on purpose. This one is used by the ECS agent
# before the container starts; the task role is what the container itself holds.
# Conflating them would hand every session ECR and CloudWatch permissions it has
# no use for.

resource "aws_iam_role" "execution" {
  name               = "${var.name_prefix}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  tags               = var.tags
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}
