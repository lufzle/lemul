variable "name_prefix" {
  description = "Prefix for every resource this module creates."
  type        = string
  default     = "lemul"
}

variable "vpc_id" {
  description = "VPC the runner and workspace tasks run in."
  type        = string
}

variable "subnet_ids" {
  description = "Subnets for task ENIs. They need a route out: the supervisor dials the control plane and the task pulls its image. Private subnets need a NAT gateway or the interface endpoints below."
  type        = list(string)
}

variable "route_table_ids" {
  description = "Route tables to attach the S3 gateway endpoint to. Only used when enable_s3_endpoint is true."
  type        = list(string)
  default     = []
}

variable "assign_public_ip" {
  description = "Assign public IPs to tasks. Required on public subnets without a NAT gateway; set false for private subnets."
  type        = bool
  default     = false
}

variable "workspace_image" {
  description = "The sandbox image. Empty uses the ECR repository this module creates. Build it with image/build.sh -- never a raw docker build, which silently ships a stale supervisor."
  type        = string
  default     = ""
}

variable "runner_image" {
  description = "Image containing the runner binary. Empty uses the ECR repository this module creates."
  type        = string
  default     = ""
}

variable "image_tag" {
  description = "Tag to deploy from the module's own ECR repositories."
  type        = string
  default     = "dev"
}

variable "ecr_force_delete" {
  description = "Allow `terraform destroy` to remove repositories that still hold images. Convenient in a test account, wrong in production."
  type        = bool
  default     = false
}

variable "cpu_architecture" {
  description = "ARM64 or X86_64. ARM64 is Graviton -- cheaper, and a native build on an Apple Silicon machine. It MUST match the architecture the images were built for, or tasks fail at start with an exec format error."
  type        = string
  default     = "ARM64"
}

variable "container_name" {
  description = "Container name in the workspace task definition. The driver addresses overrides by name, and a mismatch is accepted by the API then silently ignored at launch."
  type        = string
  default     = "workspace"
}

variable "workspace_cpu" {
  description = "Fargate CPU units per workspace task. Section 2.4 sizes 2 vCPU / 8 GiB at roughly 2-4 sessions plus a dev server."
  type        = string
  default     = "2048"
}

variable "workspace_memory" {
  description = "Fargate memory (MiB) per workspace task. Fixed at launch: a task cannot grow, which is why admission control refuses at session-create rather than letting OOM be the discovery mechanism (section 2.4)."
  type        = string
  default     = "8192"
}

variable "workspace_ephemeral_gib" {
  description = "Ephemeral disk per workspace task. 20 is the Fargate floor and fills fast with npm and build caches."
  type        = number
  default     = 50
}

variable "runner_desired_count" {
  description = "Runner replicas. Decision #11: design for N, deploy 1. The runner is control-only, so losing it means 'cannot create a workspace' for under a minute rather than lost sessions."
  type        = number
  default     = 1
}

variable "control_plane_url" {
  description = "wss:// base URL the runner dials out to. Outbound only -- nothing in this account listens (section 2.2)."
  type        = string
}

variable "organization_id" {
  description = "UUID of the organization whose workspaces this runner places. A runner serves exactly ONE, and its credential is derived for it -- get both from `controlplane -print-runner-env <org-slug>`."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-fA-F-]{36}$", var.organization_id))
    error_message = "organization_id is the organization's uuid, not its slug. `controlplane -print-runner-env <slug>` prints both."
  }
}

variable "runner_token" {
  description = "This organization's runner credential, delivered via Secrets Manager. Derived as HMAC(signing key, organization) rather than shared across the fleet, so it authorises this organization and no other. Sensitive, and note it still lands in Terraform STATE -- encrypt and access-control state, or rotate out of band after the first apply. Rotating it means rolling the control plane's signing key, which rotates every derived credential at once."
  type        = string
  sensitive   = true
}

variable "gateway_url" {
  description = <<-EOT
    UNUSED by the workspace task definition, deliberately, and kept only so an
    existing tfvars does not break.

    The Management API states the inference path once, at placement, sending the
    URL and the key together. Setting it here as well made it a second source
    for one fact, and produced a task holding a URL with no key -- which fails
    at the user's first message rather than at placement.
  EOT
  type        = string
  default     = ""
}

variable "gateway_key_secret_arn" {
  description = "Secrets Manager ARN holding the gateway credential. Empty omits it. It reaches the task as an environment secret, which is why sessions run at their own uid (section 12.6)."
  type        = string
  default     = ""
}

variable "enable_bedrock" {
  description = "Attach Bedrock invoke permissions to the workspace task role. Off by default: decision #12 makes the gateway the supported path, so the default task needs no AWS model permissions at all."
  type        = bool
  default     = false
}

variable "bedrock_model_arns" {
  description = "Model and inference-profile ARNs the task role may invoke. Both are needed: an inference profile is invoked by its own ARN and fans out to regional model ARNs, so listing only one fails at runtime."
  type        = list(string)
  default     = []
}

variable "enable_bedrock_endpoint" {
  description = "Create the bedrock-runtime interface endpoint. Costs per AZ and is pointless on the gateway path; matters for direct-to-Bedrock or a customer refusing public egress."
  type        = bool
  default     = false
}

variable "enable_s3_endpoint" {
  description = "Create the S3 gateway endpoint. Needed for ECR image pulls from a private subnet with no NAT, because ECR stores layers in S3. No hourly charge; needs route_table_ids."
  type        = bool
  default     = false
}

variable "log_retention_days" {
  description = "CloudWatch log retention."
  type        = number
  default     = 30
}

variable "secret_recovery_days" {
  description = "Secrets Manager recovery window. 0 allows immediate deletion, which is convenient in test accounts and wrong in production."
  type        = number
  default     = 7
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = {}
}

# --- The workspace volume (section 9 item 0) --------------------------------

variable "workspace_volume_gib" {
  description = <<-EOT
    Size of a FRESH workspace EBS volume, mounted at /workspace. 0 disables the
    volume entirely, which is what every deployment before this had -- and it
    means the auto-stop cascade destroys every member's home one warm hold
    after the last session ends, because /workspace is then the container's
    writable layer.

    A restored workspace takes its snapshot's size instead, so growing this only
    affects workspaces created afterwards.
  EOT
  type        = number
  default     = 30
}

variable "workspace_volume_name" {
  description = <<-EOT
    Name of the configure_at_launch volume, matched by ECS between the task
    definition and RunTask. A mismatch is not an error: ECS mounts nothing and
    the workspace quietly loses its disk on every stop.
  EOT
  type        = string
  default     = "workspace"
}
