locals {
  private_api_hostname = "jobs.${var.name}.internal"
  alb_arn_prefix       = "arn:aws:elasticloadbalancing:${var.region}:${var.expected_account_id}"
  alb_resource_arns = [for suffix in [
    "loadbalancer/app/*/*", "targetgroup/*/*", "listener/app/*/*/*", "listener-rule/app/*/*/*"
  ] : "${local.alb_arn_prefix}:${suffix}"]

  # Reviewed v3.5.0 reference, not a live URL. Keep ELB actions and discovery;
  # remove SG mutation, IAM, WAF/Shield/Cognito and unused WebACL operations.
  # Terraform owns SG rules. An operator must prepare the ELB service-linked role.
  alb_reference = jsondecode(file("${path.module}/reference/alb-iam-v3.5.0.json"))
  alb_selected = [for statement in local.alb_reference.Statement : merge(statement, {
    Action = [for action in statement.Action : action if(
      (startswith(action, "elasticloadbalancing:") && !contains([
        "elasticloadbalancing:SetWebAcl", "elasticloadbalancing:ModifyCapacityReservation", "elasticloadbalancing:ModifyIpPools"
      ], action)) || startswith(action, "ec2:Describe") || startswith(action, "ec2:Get") ||
      contains(["acm:ListCertificates", "acm:DescribeCertificate"], action)
    )]
  })]
  alb_statements = [for statement in local.alb_selected : merge(statement, {
    Resource = flatten([for arn in try(tolist(statement.Resource), [statement.Resource]) :
      arn == "*" && alltrue([for action in statement.Action : startswith(action, "elasticloadbalancing:") && !startswith(action, "elasticloadbalancing:Describe")])
      ? local.alb_resource_arns : [replace(arn, "arn:aws:elasticloadbalancing:*:*", local.alb_arn_prefix)]
      if !strcontains(arn, "/net/")
    ])
    Condition = merge(try(statement.Condition, {}), {
      StringEquals = merge(try(statement.Condition.StringEquals, {}), {
        "aws:RequestedRegion" = var.region
        }, { for key, value in try(statement.Condition.Null, {}) : key => var.name if value == "false" }
      )
    })
  }) if length(statement.Action) > 0]
}

resource "aws_iam_role" "alb_controller" {
  name = "${var.name}-alb-controller"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = { StringEquals = {
        "aws:RequestTag/eks-cluster-arn"            = aws_eks_cluster.lab.arn
        "aws:RequestTag/kubernetes-namespace"       = "kube-system"
        "aws:RequestTag/kubernetes-service-account" = "aws-load-balancer-controller"
      } }
    }]
  })
}

resource "aws_iam_role_policy" "alb_controller" {
  name = "internal-alb-controller"
  role = aws_iam_role.alb_controller.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(local.alb_statements, [{
      Sid       = "DenyInternetFacingCreation"
      Effect    = "Deny"
      Action    = ["elasticloadbalancing:CreateLoadBalancer"]
      Resource  = ["*"]
      Condition = { StringNotEquals = { "elasticloadbalancing:Scheme" = "internal" } }
    }])
  })
}

resource "aws_eks_pod_identity_association" "alb_controller" {
  cluster_name    = aws_eks_cluster.lab.name
  namespace       = "kube-system"
  service_account = "aws-load-balancer-controller"
  role_arn        = aws_iam_role.alb_controller.arn
  depends_on      = [aws_eks_addon.pod_identity_agent, aws_iam_role_policy.alb_controller]
}

resource "aws_security_group" "alb" {
  name_prefix = "${var.name}-alb-"
  description = "Internal HTTPS from the approved operator, API port to nodes only"
  vpc_id      = aws_vpc.lab.id
}

resource "aws_vpc_security_group_ingress_rule" "operator_to_alb" {
  security_group_id            = aws_security_group.alb.id
  referenced_security_group_id = aws_security_group.operator.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_egress_rule" "operator_to_alb" {
  security_group_id            = aws_security_group.operator.id
  referenced_security_group_id = aws_security_group.alb.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_egress_rule" "alb_to_api" {
  security_group_id            = aws_security_group.alb.id
  referenced_security_group_id = aws_security_group.nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 8085
  to_port                      = 8085
}

resource "aws_vpc_security_group_ingress_rule" "alb_to_api" {
  security_group_id            = aws_security_group.nodes.id
  referenced_security_group_id = aws_security_group.alb.id
  ip_protocol                  = "tcp"
  from_port                    = 8085
  to_port                      = 8085
}

# Second-stage input only, after the controller has created the ALB. Terraform
# reads it; never import it here or create an aws_lb resource for the same ALB.
data "aws_lb" "published" {
  count = var.published_alb_arn == null ? 0 : 1
  arn   = var.published_alb_arn
}

resource "aws_route53_record" "api" {
  count           = var.published_alb_arn == null ? 0 : 1
  zone_id         = aws_route53_zone.lab.zone_id
  name            = local.private_api_hostname
  type            = "A"
  allow_overwrite = false
  alias {
    name                   = data.aws_lb.published[0].dns_name
    zone_id                = data.aws_lb.published[0].zone_id
    evaluate_target_health = true
  }
  lifecycle {
    precondition {
      condition = (
        data.aws_lb.published[0].internal && data.aws_lb.published[0].load_balancer_type == "application" &&
        data.aws_lb.published[0].vpc_id == aws_vpc.lab.id &&
        toset(data.aws_lb.published[0].subnets) == toset([for subnet in aws_subnet.private : subnet.id]) &&
        toset(data.aws_lb.published[0].security_groups) == toset([aws_security_group.alb.id]) &&
        lookup(data.aws_lb.published[0].tags, "elbv2.k8s.aws/cluster", "") == var.name &&
        lookup(data.aws_lb.published[0].tags, "ingress.k8s.aws/stack", "") == "durable-jobs-aws/api"
      )
      error_message = "Refuse DNS publication: ALB must be internal, owned by this cluster's API Ingress, and use the expected VPC, private subnets and security group."
    }
  }
}
