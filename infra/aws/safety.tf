data "aws_caller_identity" "current" {}

# A second account check complements provider.allowed_account_ids. All resource
# roots depend on this gate. Mocked tests deliberately use a fictional account.
resource "terraform_data" "deployment_gate" {
  input = var.expected_account_id

  lifecycle {
    precondition {
      condition     = var.deployment_approved
      error_message = "Deployment is disabled. Review account, region, costs, private access, state and teardown first."
    }
    precondition {
      condition     = data.aws_caller_identity.current.account_id == var.expected_account_id
      error_message = "Authenticated account does not match the explicitly approved account."
    }
  }
}
