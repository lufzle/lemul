# The VENDOR side's host. One instance, one address, the compose stack on it.
#
# Separate state and a separate directory from ../../terraform, which is the
# module a CUSTOMER applies in THEIR account. Nothing there listens inbound;
# everything here does. Keeping them apart is what lets a customer read that
# module and find nothing of ours running in their VPC (section 2.2).

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

# Amazon Linux 2023, arm64, resolved rather than pinned to an id that rots.
#
# Looked up by name rather than through the /aws/service/ami-al2023-latest SSM
# public parameter, which this account cannot read -- it answers
# "No access to /aws/ namespace", which reads like a broken path rather than a
# policy. most_recent plus an owner of `amazon` is not a weaker guarantee: the
# owner is the trust anchor either way.
data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-2023*-arm64"]
  }
  filter {
    name   = "state"
    values = ["available"]
  }
}

# --- Images -----------------------------------------------------------------

resource "aws_ecr_repository" "vendor" {
  for_each = toset(["controlplane", "relay", "console"])
  # image_prefix, NOT name_prefix. The repository name is what the compose file
  # names in its `image:` lines, so it is part of the stack's own vocabulary
  # rather than of this module's resource naming -- and it reads alongside the
  # customer module's lemul-workspace and lemul-runner, which is where anyone
  # looking at the account will expect to find it.
  name                 = "${var.image_prefix}-${each.key}"
  image_tag_mutability = "MUTABLE"
  force_delete         = var.ecr_force_delete
  tags                 = var.tags

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "vendor" {
  for_each   = aws_ecr_repository.vendor
  repository = each.value.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Expire untagged images"
      selection    = { tagStatus = "untagged", countType = "imageCountMoreThan", countNumber = 3 }
      action       = { type = "expire" }
    }]
  })
}

# --- The stack's own files --------------------------------------------------
#
# Carried in S3 rather than in user_data, which is capped at 16 KB. The five
# files are 36 KB raw and 13.4 KB gzipped -- it fits, barely, and "barely" is
# the problem: the next comment added to the compose file would push it over
# and fail the apply with a length error that names nothing about the cause.
#
# The bucket is private, encrypted, and readable by exactly one role.

resource "aws_s3_bucket" "stack" {
  bucket        = "${var.name_prefix}-stack-${data.aws_caller_identity.current.account_id}"
  force_destroy = true
  tags          = var.tags
}

resource "aws_s3_bucket_public_access_block" "stack" {
  bucket                  = aws_s3_bucket.stack.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "stack" {
  bucket = aws_s3_bucket.stack.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_versioning" "stack" {
  bucket = aws_s3_bucket.stack.id
  versioning_configuration {
    status = "Enabled"
  }
}

# etag on content, so `terraform apply` after editing any of these uploads the
# new copy -- and user_data_replace_on_change then rebuilds the host with it.
resource "aws_s3_object" "stack" {
  for_each = {
    "docker-compose.yml"     = "${path.module}/../docker-compose.yml"
    "Caddyfile"              = "${path.module}/../Caddyfile"
    "up.sh"                  = "${path.module}/../up.sh"
    "initdb/01-databases.sh" = "${path.module}/../initdb/01-databases.sh"
    "litellm.yaml"           = "${path.module}/../litellm.yaml"
    "auth-stack/seed.ts"     = "${path.module}/../../auth-stack/seed.ts"
  }
  bucket = aws_s3_bucket.stack.id
  key    = each.key
  source = each.value
  etag   = filemd5(each.value)
  tags   = var.tags
}

# --- Mail -------------------------------------------------------------------
#
# SES in SANDBOX mode, which is the default for a new account and is exactly
# what is wanted here: it will only send to VERIFIED addresses, so a
# misconfiguration cannot mail a stranger. The cost is that every recipient must
# be verified first, and verification is a link AWS mails to that address --
# Terraform can request it but nobody can click it on your behalf.
#
# This replaces Mailpit, and the reason is not tidiness. Sign-in is passwordless
# email, so whoever can read the inbox can sign in as anyone; Mailpit has no
# authentication at all, which made it the weakest thing in the deployment the
# moment the deployment became reachable.

resource "aws_ses_email_identity" "verified" {
  for_each = toset(var.verified_emails)
  email    = each.value
}

# SES SMTP credentials are an IAM access key put through a signing algorithm --
# they are not an IAM password and cannot be typed in. Terraform derives the
# SMTP form directly, which is why this is a user rather than a role: a role's
# temporary credentials cannot be converted into a static SMTP password, and
# Logto's connector speaks SMTP.
resource "aws_iam_user" "smtp" {
  name = "${var.name_prefix}-smtp"
  tags = var.tags
}

data "aws_iam_policy_document" "smtp" {
  statement {
    # The only thing it can do. Not ses:* -- this credential lives in a config
    # file on a public host, so it should be able to send mail and nothing else,
    # not manage identities or read the account's sending statistics.
    actions   = ["ses:SendRawEmail"]
    resources = ["*"]
  }
}

resource "aws_iam_user_policy" "smtp" {
  name   = "${var.name_prefix}-smtp"
  user   = aws_iam_user.smtp.name
  policy = data.aws_iam_policy_document.smtp.json
}

resource "aws_iam_access_key" "smtp" {
  user = aws_iam_user.smtp.name
}

# --- Network ----------------------------------------------------------------

resource "aws_security_group" "host" {
  name        = "${var.name_prefix}-vendor"
  description = "Vendor host: HTTP and HTTPS in, nothing else"
  vpc_id      = var.vpc_id
  tags        = var.tags
}

# 80 is not optional and is not a convenience: Caddy answers the ACME HTTP-01
# challenge on it, so closing it means no certificate. It serves nothing else --
# Caddy redirects every other request to HTTPS.
resource "aws_vpc_security_group_ingress_rule" "http" {
  security_group_id = aws_security_group.host.id
  description       = "ACME HTTP-01, and the redirect to HTTPS"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 80
  to_port           = 80
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_ingress_rule" "https" {
  security_group_id = aws_security_group.host.id
  description       = "console, management API, relay, and the identity provider"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
}

# THERE IS NO SSH RULE, and that is the security posture rather than an
# omission. Shell access is Session Manager, which the agent establishes
# OUTBOUND -- so there is no management port to find, no key to lose, and every
# session is logged against an IAM principal. `make ssh` in the README.
resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.host.id
  description       = "ACME, ECR, SES, and image pulls"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

# --- The host's identity ----------------------------------------------------

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "host" {
  name               = "${var.name_prefix}-vendor-host"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
  tags               = var.tags
}

# Session Manager. This is what replaces an SSH key and an open port 22.
resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.host.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# Pulling its own images, and nothing more. Explicitly NOT the managed
# ReadOnlyAccess or PowerUser policies: this host is internet-facing, so its
# instance credentials are the thing an attacker reaches for first, and they
# should open three repositories rather than the account.
data "aws_iam_policy_document" "ecr_pull" {
  statement {
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    actions = [
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchCheckLayerAvailability",
    ]
    resources = [for r in aws_ecr_repository.vendor : r.arn]
  }
  # Its own stack files, and only those. Not s3:* and not the bucket's own
  # configuration -- this role should be able to read five objects.
  statement {
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.stack.arn}/*"]
  }
}

resource "aws_iam_role_policy" "ecr_pull" {
  name   = "${var.name_prefix}-vendor-ecr-pull"
  role   = aws_iam_role.host.id
  policy = data.aws_iam_policy_document.ecr_pull.json
}

resource "aws_iam_instance_profile" "host" {
  name = "${var.name_prefix}-vendor-host"
  role = aws_iam_role.host.name
}

# --- The address ------------------------------------------------------------
#
# Allocated BEFORE the instance and associated after, because the whole
# configuration is derived from it: BASE is <this address>.sslip.io, and the
# instance cannot be told its own name unless the name exists first. A
# public_ip assigned at launch would change on every stop/start and take every
# certificate and every OIDC issuer with it.

resource "aws_eip" "host" {
  domain = "vpc"
  tags   = merge(var.tags, { Name = "${var.name_prefix}-vendor" })
}

resource "aws_instance" "host" {
  ami                    = data.aws_ami.al2023.id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = [aws_security_group.host.id]
  iam_instance_profile   = aws_iam_instance_profile.host.name
  tags                   = merge(var.tags, { Name = "${var.name_prefix}-vendor" })

  # IMDSv2 REQUIRED, not merely available. The v1 endpoint answers an
  # unauthenticated GET, so any request-forgery bug in anything served here
  # would read this host's role credentials straight out of the metadata
  # service. Requiring a token turns that class of bug into nothing.
  metadata_options {
    http_tokens                 = "required"
    http_endpoint               = "enabled"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_size = var.disk_gib
    volume_type = "gp3"
    # Holds signing.key and the database. Every workspace, attach and runner
    # credential in every customer account derives from that key.
    encrypted = true
    tags      = var.tags
  }

  # Rebuild the host when the bootstrap changes. The stack files live in S3 and
  # their object versions are folded in below, so editing the compose file also
  # counts as a change here rather than silently leaving the host on the old one.
  user_data_replace_on_change = true
  user_data_base64 = base64gzip(templatefile("${path.module}/user-data.sh.tftpl", {
    base             = "${aws_eip.host.public_ip}.sslip.io"
    registry         = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${data.aws_region.current.region}.amazonaws.com"
    region           = data.aws_region.current.region
    image_tag        = var.image_tag
    acme_email       = var.acme_email
    smtp_host        = "email-smtp.${data.aws_region.current.region}.amazonaws.com"
    smtp_user        = aws_iam_access_key.smtp.id
    smtp_pass        = aws_iam_access_key.smtp.ses_smtp_password_v4
    smtp_from        = var.mail_from
    signup_allowlist = join(",", var.signup_allowlist)
    bedrock_api_key  = var.bedrock_api_key
    pins             = var.pins
    gateway_url      = var.gateway_url
    gateway_key      = var.gateway_key
    bucket           = aws_s3_bucket.stack.id
    stack_version    = md5(join(",", [for o in aws_s3_object.stack : o.etag]))
  }))
}

resource "aws_eip_association" "host" {
  instance_id   = aws_instance.host.id
  allocation_id = aws_eip.host.id
}
