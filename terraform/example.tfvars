# Minimal deployment on the gateway path (decision #12), which needs no Bedrock
# permissions at all. Copy, fill in, and `terraform apply -var-file=...`.

control_plane_url = "wss://control.example.com"
vpc_id            = "vpc-xxxxxxxx"
subnet_ids        = ["subnet-aaaa", "subnet-bbbb"]

# Public subnets without a NAT gateway need this to reach the registry.
assign_public_ip = true

workspace_image = "<account>.dkr.ecr.<region>.amazonaws.com/lemul-workspace:<tag>"
runner_image    = "<account>.dkr.ecr.<region>.amazonaws.com/lemul-runner:<tag>"

gateway_url = "https://litellm.internal:4000"

# Create this secret out of band; the module only references it by ARN, so the
# gateway credential never enters Terraform state.
gateway_key_secret_arn = "arn:aws:secretsmanager:<region>:<account>:secret:lemul-gateway-key-xxxxxx"

# A runner serves ONE organization and proves which with a credential derived
# for it. Both come from the control plane, in one command:
#
#   controlplane -print-runner-env <org-slug>
#
# The token DOES land in Terraform state -- encrypt and access-control state, or
# rotate out of band after the first apply.
organization_id = "00000000-0000-0000-0000-000000000000"
runner_token    = "change-me"

# Direct-to-Bedrock is a draft (section 12.5). If you enable it, list BOTH the
# inference-profile ARNs and the regional model ARNs they fan out to: an
# inference profile is invoked by its own ARN and dispatches to the model ARNs,
# so naming only one fails at runtime.
# enable_bedrock     = true
# bedrock_model_arns = [
#   "arn:aws:bedrock:us-east-2:<account>:inference-profile/us.anthropic.claude-opus-4-5-*",
#   "arn:aws:bedrock:us-east-2::foundation-model/anthropic.claude-opus-4-5-*",
# ]

tags = {
  Project = "lemul"
}
