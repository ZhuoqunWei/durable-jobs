variable "deployment_approved" {
  description = "Explicit cost/account/access review acknowledgement, not a substitute for IAM or user authorization."
  type        = bool
  default     = false
}

variable "lab_run_id" {
  description = "Unique live-run ownership marker used by the independent cleanup controller."
  type        = string
  default     = "offline-validation"
  validation {
    condition     = var.lab_run_id == "offline-validation" || can(regex("^lab-[0-9]{8}T[0-9]{6}Z$", var.lab_run_id))
    error_message = "Use a timestamped lab run ID; offline-validation is for mocked tests only."
  }
}

variable "operator_ami_id" {
  description = "Reviewed Amazon Linux 2023 x86_64 AMI for an optional private SSM operator. Null creates none."
  type        = string
  default     = null
  validation {
    condition     = var.operator_ami_id == null ? true : can(regex("^ami-[a-f0-9]{17}$", var.operator_ami_id))
    error_message = "Use a reviewed explicit AMI ID or null."
  }
}

variable "expected_account_id" {
  description = "The exact commercial AWS account approved for a future lab. No default."
  type        = string
  validation {
    condition     = can(regex("^[0-9]{12}$", var.expected_account_id))
    error_message = "Supply the approved 12-digit AWS account ID."
  }
}

variable "region" {
  description = "Explicitly selected commercial US region; other partitions are outside this lab."
  type        = string
  validation {
    condition     = can(regex("^us-(east|west)-[12]$", var.region))
    error_message = "Choose a supported commercial US region after reviewing price and availability."
  }
}

variable "availability_zones" {
  description = "Two distinct AZ names in the selected region, checked for availability before any real run."
  type        = list(string)
  validation {
    condition = length(var.availability_zones) == 2 && length(toset(var.availability_zones)) == 2 && alltrue([
      for az in var.availability_zones : can(regex("^${var.region}[a-z]$", az))
    ])
    error_message = "Supply exactly two distinct Availability Zones in the selected region."
  }
}

variable "operator_role_arn" {
  description = "An existing, explicitly approved IAM role for lab cluster administration; never a root or user ARN."
  type        = string
  validation {
    condition     = can(regex("^arn:aws:iam::${var.expected_account_id}:role/[A-Za-z0-9+=,.@_/-]+$", var.operator_role_arn))
    error_message = "Supply an existing IAM role ARN in the approved account. This grants cluster-admin access."
  }
}

variable "name" {
  type    = string
  default = "durable-jobs-lab"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}[a-z0-9]$", var.name))
    error_message = "Use 3–32 lowercase letters, digits and hyphens, starting with a letter and ending with a letter/digit for the private hostname."
  }
}

variable "vpc_cidr" {
  description = "A canonical private /16 in 10/8. Check overlap with connected networks before deployment."
  type        = string
  default     = "10.42.0.0/16"
  validation {
    condition     = can(cidrnetmask(var.vpc_cidr)) && can(regex("^10\\.[0-9]{1,3}\\.0\\.0/16$", var.vpc_cidr))
    error_message = "Use a valid canonical CIDR such as 10.42.0.0/16."
  }
}

variable "nat_mode" {
  description = "single saves gateway hours but shares an AZ failure domain; per_az provides AZ-local egress at higher cost."
  type        = string
  default     = "single"
  validation {
    condition     = contains(["single", "per_az"], var.nat_mode)
    error_message = "nat_mode must be single or per_az."
  }
}

variable "kubernetes_version" {
  description = "Reviewed EKS minor version. Recheck AWS standard-support dates before a real deployment."
  type        = string
  default     = "1.35"
  validation {
    condition     = var.kubernetes_version == "1.35"
    error_message = "This lab currently tests 1.35. Review support and update code/tests together before changing it."
  }
}

variable "addon_versions" {
  description = "Exact EKS add-on builds, checked against this region, Kubernetes minor and AMD64 before deployment. No inferred latest versions."
  type = object({
    pod_identity_agent = string
    ebs_csi            = string
    vpc_cni            = string
    kube_proxy         = string
    coredns            = string
  })
  default = {
    pod_identity_agent = ""
    ebs_csi            = ""
    vpc_cni            = ""
    kube_proxy         = ""
    coredns            = ""
  }
  nullable = false
  validation {
    condition = alltrue([
      for version in values(var.addon_versions) : can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+-eksbuild\\.[0-9]+$", version))
    ])
    error_message = "Choose all five exact add-on builds (vX.Y.Z-eksbuild.N) after AWS compatibility review. Empty or mutable versions are not deployable."
  }
}

variable "published_alb_arn" {
  description = "Optional second-stage ARN of the controller-created internal ALB. Null creates no alias; never supply a guessed ARN."
  type        = string
  default     = null
  validation {
    condition = var.published_alb_arn == null ? true : can(regex(
      "^arn:aws:elasticloadbalancing:${var.region}:${var.expected_account_id}:loadbalancer/app/[A-Za-z0-9-]+/[a-f0-9]{16}$", var.published_alb_arn
    ))
    error_message = "Use the actual Application Load Balancer ARN in the approved account and region, or leave null."
  }
}
