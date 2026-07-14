output "cluster_arn" {
  description = "Cluster the runner places workspace tasks in."
  value       = aws_ecs_cluster.this.arn
}

output "workspace_task_definition" {
  description = "Family the runner's IAM policy names. Pass the family, not a revision, so a new image is a new revision rather than an IAM change."
  value       = aws_ecs_task_definition.workspace.family
}

output "runner_role_arn" {
  description = "Control-only. Holds ecs:RunTask/StopTask on one family and no Bedrock (section 2.1)."
  value       = aws_iam_role.runner.arn
}

output "workspace_task_role_arn" {
  description = "What a session actually runs as. Holds no ecs:* -- it cannot place or stop tasks (section 2.1)."
  value       = aws_iam_role.workspace_task.arn
}

output "workspace_security_group_id" {
  description = "Egress-only. Nothing dials in."
  value       = aws_security_group.workspace.id
}

output "runner_token_secret_arn" {
  description = "Rotate by updating this secret then forcing a new runner deployment; running sessions are untouched, because the runner is off the data path."
  value       = aws_secretsmanager_secret.runner_token.arn
}

# The workspace volume, output because the runner needs both when it is run BY
# HAND with -driver ecs -- which is how the last Fargate round was done, and the
# mode where none of the runner task definition's environment exists.
#
# Getting these wrong is silent in the worst way: an empty volume name disables
# the volume entirely, so the workspace comes back blank and the run "passes".
output "workspace_volume_name" {
  description = "Pass as -ecs-volume-name (or LEMUL_ECS_VOLUME_NAME) when running the runner by hand."
  value       = var.workspace_volume_gib > 0 ? var.workspace_volume_name : ""
}

output "volume_infrastructure_role_arn" {
  description = "Pass as -ecs-infrastructure-role (or LEMUL_ECS_INFRASTRUCTURE_ROLE). ECS assumes this to create and attach the workspace volume; the runner may only pass it."
  value       = var.workspace_volume_gib > 0 ? aws_iam_role.volume_infrastructure[0].arn : ""
}
