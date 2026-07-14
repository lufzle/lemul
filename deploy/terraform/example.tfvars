# Copy to <name>.auto.tfvars and fill in. *.tfvars is gitignored.

vpc_id    = "vpc-xxxxxxxx"
subnet_id = "subnet-xxxxxxxx" # must be PUBLIC: the EIP needs an IGW route,
                              # and Let's Encrypt validates over port 80

# SES is in sandbox mode, so it delivers ONLY to verified addresses -- every
# person who signs in must be listed, and so must mail_from. Each gets a link
# from AWS that they have to click.
verified_emails = ["you@example.com"]
mail_from       = "you@example.com"

acme_email = "you@example.com"

# Who may create an account. Empty means ANYONE.
signup_allowlist = ["you@example.com"]

# Convenient in a test account, wrong in production.
ecr_force_delete = true

tags = {
  Project   = "lemul"
  ManagedBy = "terraform"
}
