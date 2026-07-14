output "base" {
  description = "The hostname suffix everything is derived from. No DNS to configure: sslip.io reads the address out of the name."
  value       = "${aws_eip.host.public_ip}.sslip.io"
}

output "console_url" {
  description = "The operator console. Sign in with an address SES has verified."
  value       = "https://console.${aws_eip.host.public_ip}.sslip.io"
}

output "control_plane_url" {
  description = "What a runner dials out to. This is the value for control_plane_url in the CUSTOMER module (../../terraform)."
  value       = "wss://cp.${aws_eip.host.public_ip}.sslip.io"
}

output "registry" {
  description = "Push the three vendor images here: REGISTRY=<this> PUSH=1 ../build.sh"
  value       = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${data.aws_region.current.region}.amazonaws.com"
}

output "instance_id" {
  description = "For a shell: aws ssm start-session --target <this>. There is no SSH port and no key."
  value       = aws_instance.host.id
}

output "pending_email_verifications" {
  description = "SES is in sandbox mode, so each of these must click the link AWS mailed them before any sign-in code can be delivered."
  value       = var.verified_emails
}
