variable "name_prefix" {
  description = "Prefix for every resource this module creates. Deliberately different from the customer module's, so the two are never confused in one account."
  type        = string
  default     = "lemul-vendor"
}

variable "vpc_id" {
  description = "VPC for the host. The default VPC is fine -- this instance needs a public address and a route out, and nothing else."
  type        = string
}

variable "subnet_id" {
  description = "PUBLIC subnet. The Elastic IP has to attach to something with an internet gateway route, and Let's Encrypt must be able to reach port 80 to validate."
  type        = string
}

variable "instance_type" {
  description = <<-EOT
    Graviton, to match the arm64 images build.sh produces on an Apple Silicon
    machine.

    t4g.MEDIUM (4 GiB), raised from small on 2026-08-03 after the gateway went
    on the box and wedged it. LiteLLM is a Python service that wants roughly a
    gigabyte on its own, next to Postgres, Logto, the console's Bun runtime,
    two Go services and a proxy -- 2 GiB did not hold it. Measured rather than
    guessed: CPUUtilization went from ~3% to a sustained 70-85% the minute
    LiteLLM started, CPUCreditBalance fell to 0.0 within fifteen minutes, and
    the host stopped answering SSM entirely.
  EOT
  type        = string
  default     = "t4g.medium"
}

variable "disk_gib" {
  description = "Root volume. Holds the database, the container images and the signing key."
  type        = number
  default     = 30
}

variable "image_prefix" {
  description = "Repository name prefix for the three vendor images. Must match what deploy/docker-compose.yml names in its image: lines -- they are the same string in two places, and a mismatch is an ImagePullBackOff that names a repository nobody created."
  type        = string
  default     = "lemul"
}

variable "image_tag" {
  description = "Tag to run from the module's own ECR repositories."
  type        = string
  default     = "dev"
}

variable "ecr_force_delete" {
  description = "Allow `terraform destroy` to remove repositories that still hold images. Convenient in a test account, wrong in production."
  type        = bool
  default     = false
}

variable "acme_email" {
  description = "Where Let's Encrypt sends expiry warnings. Empty is accepted and Caddy still issues certificates."
  type        = string
  default     = ""
}

variable "verified_emails" {
  description = <<-EOT
    Addresses SES is asked to verify. In SANDBOX mode -- the default, and what
    this deployment wants -- SES will only send TO a verified address, so every
    person who signs in must appear here, and so must mail_from.

    Verification is a link AWS emails to each address. Terraform requests it;
    somebody has to click it, and until they do, sign-in codes are accepted by
    SES and silently dropped.
  EOT
  type        = list(string)
}

variable "mail_from" {
  description = "Envelope sender for sign-in codes. In sandbox mode this must ITSELF be verified, so it normally appears in verified_emails too."
  type        = string
}

variable "signup_allowlist" {
  description = <<-EOT
    Who may create an account. Exact addresses, whole domains, or wildcards.

    EMPTY MEANS ANYONE with a working mailbox. SES sandbox mode is not a
    substitute: it refuses delivery to unverified addresses, so sign-up appears
    closed while the actual policy is open, and it stops being true the day the
    account leaves the sandbox.
  EOT
  type        = list(string)
  default     = []
}

variable "bedrock_api_key" {
  description = <<-EOT
    Bedrock API key (ABSK...) the gateway authenticates with. A BEARER TOKEN,
    not an access key pair -- so the host needs no IAM principal that can invoke
    a model, and rotating it does not touch the instance role.

    It reaches the host through user_data and therefore lands in Terraform
    STATE, the same caveat runner_token carries in the customer module: encrypt
    and access-control state, or rotate out of band. It must be here rather than
    hand-set on the box, because a host rebuilt without it cannot start the
    gateway at all.
  EOT
  type        = string
  default     = ""
  sensitive   = true
}

variable "pins" {
  description = "Which model each role resolves to. Must name models the gateway actually serves -- the same strings appear in deploy/litellm.yaml, and a pin it does not know is a session that dies on its first prompt."
  type        = string
  default     = "opus=claude-opus-4-8,sonnet=claude-sonnet-4-6,haiku=claude-haiku-4-5"
}

variable "gateway_url" {
  description = "Where the supervisor brokers model traffic. Internal by default, because the gateway runs in this same stack; §12.4 puts it in the CUSTOMER's account in the real product."
  type        = string
  default     = "http://litellm:4000"
}

variable "gateway_key" {
  description = "Credential for the gateway above. Lands in a file on the host and in Terraform STATE -- encrypt and access-control state, or rotate out of band."
  type        = string
  default     = ""
  sensitive   = true
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = {}
}
