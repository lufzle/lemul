# Image repositories.
#
# In the module rather than left to the customer because the two images are
# versioned together with this code: the supervisor is baked into the workspace
# image, and it speaks the tunnel protocol the control plane serves. A customer
# pointing the task definition at an image they built from a different commit is
# the version-skew risk in section 13.

resource "aws_ecr_repository" "workspace" {
  name                 = "${var.name_prefix}-workspace"
  image_tag_mutability = "MUTABLE"
  force_delete         = var.ecr_force_delete
  tags                 = var.tags

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_repository" "runner" {
  name                 = "${var.name_prefix}-runner"
  image_tag_mutability = "MUTABLE"
  force_delete         = var.ecr_force_delete
  tags                 = var.tags

  image_scanning_configuration {
    scan_on_push = true
  }
}

# Untagged images accumulate on every rebuild that reuses a tag, and ECR bills
# for storage. Keep the last few in case a rollback is needed.
resource "aws_ecr_lifecycle_policy" "workspace" {
  repository = aws_ecr_repository.workspace.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Expire untagged images"
      selection = {
        tagStatus   = "untagged"
        countType   = "imageCountMoreThan"
        countNumber = 3
      }
      action = { type = "expire" }
    }]
  })
}

resource "aws_ecr_lifecycle_policy" "runner" {
  repository = aws_ecr_repository.runner.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Expire untagged images"
      selection = {
        tagStatus   = "untagged"
        countType   = "imageCountMoreThan"
        countNumber = 3
      }
      action = { type = "expire" }
    }]
  })
}

locals {
  # Empty means "use the repository this module created", which is the common
  # case; an explicit value is for a customer with their own registry.
  workspace_image = var.workspace_image != "" ? var.workspace_image : "${aws_ecr_repository.workspace.repository_url}:${var.image_tag}"
  runner_image    = var.runner_image != "" ? var.runner_image : "${aws_ecr_repository.runner.repository_url}:${var.image_tag}"
}
