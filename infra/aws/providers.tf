provider "aws" {
  region              = var.region
  allowed_account_ids = [var.expected_account_id]

  default_tags {
    tags = {
      Project   = var.name
      ManagedBy = "terraform"
      Purpose   = "personal-learning-lab"
      LabRun    = var.lab_run_id
    }
  }
}
