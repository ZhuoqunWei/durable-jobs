# Optional short-lived private access path. No SSH key, public IP or inbound SG.
# This host can read this run's deployment bundle and assume the operator role;
# it cannot provision cloud resources or read arbitrary bucket objects.
resource "aws_iam_role" "operator_host" {
  count = var.operator_ami_id == null ? 0 : 1
  name  = "${var.name}-operator-host"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "sts:AssumeRole", Principal = { Service = "ec2.amazonaws.com" } }]
  })
  depends_on = [terraform_data.deployment_gate]
}

resource "aws_iam_role_policy_attachment" "operator_ssm" {
  count      = var.operator_ami_id == null ? 0 : 1
  role       = aws_iam_role.operator_host[0].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "operator_bundle" {
  count = var.operator_ami_id == null ? 0 : 1
  name  = "exact-lab-bundle-and-operator"
  role  = aws_iam_role.operator_host[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = "sts:AssumeRole", Resource = var.operator_role_arn },
      { Effect = "Allow", Action = "s3:GetObject", Resource = "arn:aws:s3:::durable-jobs-state-${var.expected_account_id}-${var.region}/runs/${var.lab_run_id}/deploy/*" },
    ]
  })
}

resource "aws_iam_instance_profile" "operator" {
  count = var.operator_ami_id == null ? 0 : 1
  name  = "${var.name}-operator-host"
  role  = aws_iam_role.operator_host[0].name
}

resource "aws_vpc_security_group_egress_rule" "operator_https" {
  count             = var.operator_ami_id == null ? 0 : 1
  security_group_id = aws_security_group.operator.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  description       = "Short lab compromise: outbound SSM, STS, S3 and pinned tool downloads via NAT"
}

resource "aws_instance" "operator" {
  count                       = var.operator_ami_id == null ? 0 : 1
  ami                         = var.operator_ami_id
  instance_type               = "t3.micro"
  subnet_id                   = aws_subnet.private[var.availability_zones[0]].id
  vpc_security_group_ids      = [aws_security_group.operator.id]
  associate_public_ip_address = false
  iam_instance_profile        = aws_iam_instance_profile.operator[0].name
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  root_block_device {
    encrypted             = true
    volume_type           = "gp3"
    volume_size           = 8
    delete_on_termination = true
  }
  tags        = { Name = "${var.name}-operator", Role = "lab-operator", Project = var.name, LabRun = var.lab_run_id }
  volume_tags = { Project = var.name, LabRun = var.lab_run_id, Role = "lab-operator" }
  user_data   = "#!/bin/bash\nset -eu\nsystemctl enable --now amazon-ssm-agent\n"
  depends_on = [
    aws_route.private_egress,
    aws_route_table_association.private,
    aws_vpc_security_group_egress_rule.operator_https,
    aws_iam_role_policy_attachment.operator_ssm,
    aws_iam_role_policy.operator_bundle,
  ]
}

output "operator_instance_id" {
  description = "Private, temporary SSM operator; no public IP or SSH access."
  value       = try(aws_instance.operator[0].id, null)
}
