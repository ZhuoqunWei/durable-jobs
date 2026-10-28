output "vpc_id" {
  value = aws_vpc.lab.id
}

output "private_subnet_ids" {
  value = { for az, subnet in aws_subnet.private : az => subnet.id }
}

output "cluster_name" {
  value = aws_eks_cluster.lab.name
}

output "private_cluster_endpoint" {
  description = "Only reachable from an approved private access path, not directly from an ordinary laptop."
  value       = aws_eks_cluster.lab.endpoint
}

output "operator_security_group_id" {
  description = "The optional private SSM operator uses this group plus the approved operator role."
  value       = aws_security_group.operator.id
}

output "private_dns_zone_id" {
  value = aws_route53_zone.lab.zone_id
}

output "scope_notice" {
  value = "Foundation and identity configuration only; Terraform outputs alone do not prove workload health, HTTPS reachability or recovery. See the dated live-lab report for runtime evidence."
}

output "vpc_cni_role_arn" {
  description = "Dedicated aws-node Pod Identity role; not evidence of credentials successfully used in AWS."
  value       = aws_iam_role.vpc_cni.arn
}

output "ebs_csi_role_arn" {
  description = "Dedicated controller role, not an application or node role. AWS-managed policy scope still requires review."
  value       = aws_iam_role.ebs_csi.arn
}

output "configured_addon_versions" {
  description = "Explicit requested pins, not evidence of AWS compatibility or healthy running add-ons."
  value       = var.addon_versions
}

output "ingress_config" {
  description = "Non-secret input for the offline aws_ingress.py renderer after an approved foundation apply."
  value = {
    account_id         = var.expected_account_id
    region             = var.region
    cluster_name       = aws_eks_cluster.lab.name
    vpc_id             = aws_vpc.lab.id
    private_subnet_ids = [for subnet in aws_subnet.private : subnet.id]
    security_group_id  = aws_security_group.alb.id
    hostname           = local.private_api_hostname
  }
}
