# Nothing listens inbound in the customer's account (section 2.2).
#
# Both security groups are egress-only, and that is not a default we inherited --
# it is the property a security review will ask about. The runner dials out to
# the Management API; each workspace task dials out TWICE, once to the Management
# API for control and once to the relay for session bytes. There is no ingress
# rule anywhere in this file, and adding one should require an argument.
#
# The split needs no rule of its own here because egress is unrestricted below.
# It WOULD need one under a customer who allowlists destinations (1.3's A1(b)),
# and that is the case to remember: two services means two hostnames, and a task
# that can reach only one of them looks healthy on the control tunnel while
# nobody can attach to it.

resource "aws_security_group" "runner" {
  name        = "${var.name_prefix}-runner"
  description = "Runner: outbound only. Dials the control plane; never dialled."
  vpc_id      = var.vpc_id
  tags        = var.tags
}

resource "aws_security_group" "workspace" {
  name        = "${var.name_prefix}-workspace"
  description = "Workspace task: outbound only. Dials the control plane and the gateway."
  vpc_id      = var.vpc_id
  tags        = var.tags
}

# Egress is open by default because a workspace legitimately needs npm, pypi,
# git and the gateway. A customer who wants this narrowed should do it here --
# but note that Claude Code itself also phones home unless
# CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC is set (section 12.6), so an
# egress allowlist without that variable produces confusing partial failures.
resource "aws_vpc_security_group_egress_rule" "runner" {
  security_group_id = aws_security_group.runner.id
  description       = "Outbound to the Management API and the container registry"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_egress_rule" "workspace" {
  security_group_id = aws_security_group.workspace.id
  description       = "Outbound to the Management API, the relay, the gateway, and package registries"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# --- VPC endpoints ----------------------------------------------------------
#
# Optional, and off by default: they cost money per AZ and are pointless on the
# gateway path, where the workspace talks HTTPS to a gateway rather than to
# Bedrock. They matter for a direct-to-Bedrock deployment (section 12.5) and for
# a customer who refuses public egress, which is the A1(b) scenario in 1.3.

resource "aws_vpc_endpoint" "bedrock_runtime" {
  count               = var.enable_bedrock_endpoint ? 1 : 0
  vpc_id              = var.vpc_id
  service_name        = "com.amazonaws.${data.aws_region.current.region}.bedrock-runtime"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = var.subnet_ids
  security_group_ids  = [aws_security_group.workspace.id]
  private_dns_enabled = true
  tags                = var.tags
}

# S3, and the reason changed on 2026-08-02 without the resource changing.
#
# It was here for snapshot traffic; snapshots are EBS now and never touch S3.
# What still needs it is the IMAGE PULL: ECR stores layers in S3, so a task in a
# private subnet with no NAT cannot pull without this endpoint alongside the two
# ECR ones above. Removing it with the bucket would have broken private-subnet
# deployments in a way that presents as a task stuck in PENDING.
#
# A gateway endpoint rather than an interface: no hourly charge, and layer pulls
# are the one flow here with real volume.
resource "aws_vpc_endpoint" "s3" {
  count             = var.enable_s3_endpoint ? 1 : 0
  vpc_id            = var.vpc_id
  service_name      = "com.amazonaws.${data.aws_region.current.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = var.route_table_ids
  tags              = var.tags
}
