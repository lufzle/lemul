terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

data "aws_partition" "current" {}
data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

resource "aws_ecs_cluster" "this" {
  name = var.name_prefix
  tags = var.tags
}

resource "aws_cloudwatch_log_group" "workspace" {
  name              = "/${var.name_prefix}/workspace"
  retention_in_days = var.log_retention_days
  tags              = var.tags
}

resource "aws_cloudwatch_log_group" "runner" {
  name              = "/${var.name_prefix}/runner"
  retention_in_days = var.log_retention_days
  tags              = var.tags
}

# --- Where a workspace's disk lives between tasks ---------------------------
#
# An S3 bucket stood here, provisioned for section 9's recorded "snapshot to S3
# on suspend, restore on resume". Nothing ever wrote to it, and on 2026-08-02
# the medium changed to EBS snapshots, so it is gone rather than left behind in
# every customer's account.
#
# The argument was cold start, which is what section 12.10 rests on. An S3
# restore downloads and untars before the workspace is usable, on top of the
# 22.6 s placement -- and it is many small files, exactly the profile that ruled
# out EFS in section 1.1. An EBS snapshot restores lazily: the volume is
# available immediately and blocks fault in on demand. Snapshots after the first
# are block-level incremental, so cost tracks change rather than size.
#
# The volume and its snapshots are in tasks.tf and iam.tf. There is nothing to
# provision here: ECS creates the volume per task, and the snapshot that carries
# a workspace forward is created by the runner at stop time.

# --- Runner token delivery --------------------------------------------------
#
# Terraform variable -> Secrets Manager -> task env (section 8). The value is
# marked sensitive so it does not land in plan output, but note the standing
# caveat: it is still in Terraform STATE. State must be encrypted and access-
# controlled, or rotate out of band after the first apply.
#
# Rotation: update the secret version, then force a new runner deployment. The
# runner is control-only, so replacing it does not touch a running session --
# that is the property decision #6 bought (section 2.8).

resource "aws_secretsmanager_secret" "runner_token" {
  name                    = "${var.name_prefix}-runner-token"
  recovery_window_in_days = var.secret_recovery_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret_version" "runner_token" {
  secret_id     = aws_secretsmanager_secret.runner_token.id
  secret_string = var.runner_token
}
