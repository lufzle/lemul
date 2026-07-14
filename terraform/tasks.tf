# --- Workspace task definition ---------------------------------------------
#
# ONE family, which is what the runner's ecs:RunTask policy names. New images are
# new revisions of this family, so a rollout never touches IAM.
#
# No command here: the runner supplies argv as a container override at RunTask,
# because -workspace and -token differ per task and per generation.

resource "aws_ecs_task_definition" "workspace" {
  family                   = "${var.name_prefix}-workspace"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.workspace_cpu
  memory                   = var.workspace_memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.workspace_task.arn
  tags                     = var.tags

  # Must match what image/build.sh produced. A mismatch is not a warning: the
  # task starts and dies with an exec format error.
  runtime_platform {
    cpu_architecture        = var.cpu_architecture
    operating_system_family = "LINUX"
  }

  # Claude Code plus a dev server plus whatever the agent builds. Section 2.4
  # sizes 2 vCPU / 8 GiB at roughly 2-4 sessions; 20 GiB is the Fargate default
  # and the floor, raised because npm and build caches fill it fast.
  ephemeral_storage {
    size_in_gib = var.workspace_ephemeral_gib
  }

  # The workspace's durable disk (section 9 item 0).
  #
  # configure_at_launch is what lets the runner supply the size, the snapshot to
  # restore from and the termination policy per RunTask -- which it must, because
  # restoring is a property of THIS placement rather than of the definition. A
  # volume pinned in the task definition could only ever be a blank one.
  #
  # Everything durable lives under this mount: the member homes AND the shared
  # directory (section 2.3). The two used to be split across it, and the half
  # outside would have survived nothing.
  dynamic "volume" {
    for_each = var.workspace_volume_gib > 0 ? [1] : []
    content {
      name                = var.workspace_volume_name
      configure_at_launch = true
    }
  }

  container_definitions = jsonencode([{
    name      = var.container_name
    image     = local.workspace_image
    essential = true

    # A session is a PTY. Without this the container has no TTY at all and
    # isatty() fails inside it, which is the binary failure mode section 4.1
    # describes -- no TUI, no colour, line-buffered.
    pseudoTerminal = true
    # Reap orphaned processes. Claude Code spawns tool subprocesses and MCP
    # servers; without an init they accumulate as zombies in a long-lived task.
    linuxParameters = { initProcessEnabled = true }

    # Where the volume lands. ECS matches this to the volume by NAME and
    # silently mounts nothing when it does not match, so a typo here is a
    # workspace that quietly goes back to losing every home on every stop.
    mountPoints = var.workspace_volume_gib > 0 ? [{
      sourceVolume  = var.workspace_volume_name
      containerPath = "/workspace"
      readOnly      = false
    }] : []

    # NOTE: CLAUDE_CODE_SUBPROCESS_ENV_SCRUB is deliberately NOT set.
    # It needs bubblewrap, which needs namespace syscalls Fargate does not
    # permit -- measured, see section 12.6. The boundary that keeps the gateway
    # credential away from a session here is LEMUL_SESSION_UID, which needs
    # nothing from the platform.
    environment = [
      # NO LEMUL_GATEWAY_URL HERE, and its absence is the point.
      #
      # The Management API states the inference path ONCE, at placement, in the
      # container override -- URL and key together or neither. Naming the URL
      # here too made it a second source for one fact, and the two disagreed the
      # first time a control plane came up without a gateway: the task
      # definition still supplied a URL, the override supplied no key, and the
      # supervisor's proxy sent `Bearer ` with nothing after it. Claude Code
      # showed "401 Malformed API Key" on the user's first message, six layers
      # from the cause.
      #
      # Section 13 has the same shape recorded for SessionCmd, and the fix is
      # the same: the two paths cannot disagree, because there is no second
      # value.
      { name = "LEMUL_SESSION_UID", value = "1000" },
      { name = "LEMUL_SESSION_GID", value = "1000" },
      # JSON so CloudWatch Insights can query these as fields. The binaries
      # default to text, which is right on a terminal and useless in a log
      # group -- the format that is wrong here is merely ugly, the one that is
      # wrong locally makes development worse, so the default favours local.
      { name = "LEMUL_LOG_FORMAT", value = "json" },
      { name = "CLAUDE_CONFIG_DIR", value = "/workspace/.claude" },
    ]

    # The gateway credential arrives as a secret, so it is absent from
    # DescribeTaskDefinition. It still lands in the task's environment, which is
    # exactly why sessions run at their own uid (section 12.6).
    secrets = var.gateway_key_secret_arn == "" ? [] : [
      { name = "LEMUL_GATEWAY_KEY", valueFrom = var.gateway_key_secret_arn },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.workspace.name
        "awslogs-region"        = data.aws_region.current.region
        "awslogs-stream-prefix" = "workspace"
      }
    }
  }])
}

# --- Runner service ---------------------------------------------------------
#
# desiredCount 1 by decision #11: design for N, deploy 1. The runner is
# control-only, so a replacement self-heals in 30-60s and the exposure is
# "cannot create a workspace" for under a minute -- running sessions are
# untouched, because the runner is not on the data path.

resource "aws_ecs_task_definition" "runner" {
  family                   = "${var.name_prefix}-runner"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.runner.arn
  tags                     = var.tags

  runtime_platform {
    cpu_architecture        = var.cpu_architecture
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name      = "runner"
    image     = local.runner_image
    essential = true

    command = ["-driver", "ecs"]

    environment = [
      { name = "LEMUL_CONTROL_PLANE", value = var.control_plane_url },
      { name = "LEMUL_LOG_FORMAT", value = "json" },
      # Which organization this runner serves. Not a secret -- it is a uuid the
      # control plane already knows -- but it is required, and it must be the
      # SAME organization the token below is derived for: the control plane
      # verifies the pair, so a mismatch is a 401 loop rather than a misroute.
      { name = "LEMUL_TENANT_ID", value = var.organization_id },
      { name = "LEMUL_ECS_CLUSTER", value = aws_ecs_cluster.this.name },
      { name = "LEMUL_ECS_TASK_DEFINITION", value = aws_ecs_task_definition.workspace.family },
      { name = "LEMUL_ECS_SUBNETS", value = join(",", var.subnet_ids) },
      { name = "LEMUL_ECS_SECURITY_GROUPS", value = aws_security_group.workspace.id },
      { name = "LEMUL_ECS_CONTAINER_NAME", value = var.container_name },
      # Governs the WORKSPACE tasks the runner places, which is a different
      # setting from this service's own network configuration below -- they are
      # separate ENIs. Omitting it was a real failure on the first apply: the
      # workspace task attached an ENI in a public subnet with no public IP, and
      # with no NAT gateway there was no route to ECR, so the task sat in PENDING
      # with pullStartedAt never set. It does not fail, it hangs.
      { name = "LEMUL_ECS_ASSIGN_PUBLIC_IP", value = var.assign_public_ip ? "1" : "" },
      # The workspace disk. An empty volume name disables it entirely, which is
      # what every deployment before section 9 item 0 had -- and what
      # e2e.TestWithoutAVolumeTheCascadeDestroysEveryHome measures the cost of.
      { name = "LEMUL_ECS_VOLUME_NAME", value = var.workspace_volume_gib > 0 ? var.workspace_volume_name : "" },
      { name = "LEMUL_ECS_INFRASTRUCTURE_ROLE", value = var.workspace_volume_gib > 0 ? aws_iam_role.volume_infrastructure[0].arn : "" },
      { name = "LEMUL_ECS_VOLUME_GIB", value = tostring(var.workspace_volume_gib) },
    ]

    secrets = [
      { name = "LEMUL_RUNNER_TOKEN", valueFrom = aws_secretsmanager_secret.runner_token.arn },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.runner.name
        "awslogs-region"        = data.aws_region.current.region
        "awslogs-stream-prefix" = "runner"
      }
    }
  }])
}

resource "aws_ecs_service" "runner" {
  name            = "${var.name_prefix}-runner"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.runner.arn
  desired_count   = var.runner_desired_count
  launch_type     = "FARGATE"
  tags            = var.tags

  network_configuration {
    subnets          = var.subnet_ids
    security_groups  = [aws_security_group.runner.id]
    assign_public_ip = var.assign_public_ip
  }
}
