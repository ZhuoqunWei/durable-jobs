# This endpoint serves the credential exchange, NOT the Kubernetes API or EC2
# volume API. NAT remains for image pulls and the driver's EC2 API requests.
resource "aws_security_group" "eks_auth" {
  name_prefix = "${var.name}-eks-auth-"
  description = "Pod Identity credential exchange from lab nodes only"
  vpc_id      = aws_vpc.lab.id
  # No outbound rule needed for stateful responses to inbound HTTPS.
}

resource "aws_vpc_security_group_ingress_rule" "eks_auth" {
  security_group_id            = aws_security_group.eks_auth.id
  referenced_security_group_id = aws_security_group.nodes.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_endpoint" "eks_auth" {
  vpc_id              = aws_vpc.lab.id
  service_name        = "com.amazonaws.${var.region}.eks-auth"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = [for subnet in aws_subnet.private : subnet.id]
  security_group_ids  = [aws_security_group.eks_auth.id]
  private_dns_enabled = true

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = "*"
      Action    = "eks-auth:AssumeRoleForPodIdentity"
      Resource  = "*"
      Condition = { StringEquals = { "aws:PrincipalArn" = aws_iam_role.nodes.arn } }
    }]
  })
  tags = { Name = "${var.name}-eks-auth" }
}
