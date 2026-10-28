resource "aws_cloudwatch_log_group" "control_plane" {
  name              = "/aws/eks/${var.name}/cluster"
  retention_in_days = 7
  depends_on        = [terraform_data.deployment_gate]
}

resource "aws_eks_cluster" "lab" {
  name     = var.name
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version
  # Fresh clusters only: Terraform owns the managed networking/DNS add-ons.
  # Changing this flag on an existing cluster forces replacement; see docs/15.
  bootstrap_self_managed_addons = false
  enabled_cluster_log_types     = ["api", "audit", "authenticator", "controllerManager", "scheduler"]

  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = false
  }
  upgrade_policy {
    support_type = "STANDARD"
  }
  kubernetes_network_config {
    service_ipv4_cidr = "172.20.0.0/16"
  }
  vpc_config {
    subnet_ids              = [for subnet in aws_subnet.private : subnet.id]
    security_group_ids      = [aws_security_group.control_plane.id]
    endpoint_private_access = true
    endpoint_public_access  = false
  }
  depends_on = [aws_iam_role_policy_attachment.cluster, aws_cloudwatch_log_group.control_plane]
}

resource "aws_launch_template" "nodes" {
  name_prefix            = "${var.name}-nodes-"
  vpc_security_group_ids = [aws_security_group.nodes.id]
  dynamic "tag_specifications" {
    for_each = toset(["instance", "volume"])
    content {
      resource_type = tag_specifications.value
      tags          = { Project = var.name, LabRun = var.lab_run_id, Role = "managed-worker" }
    }
  }
  # Providing this custom group means EKS does not attach its permissive primary
  # cluster SG to these nodes. The connectivity rules above are therefore required.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_type           = "gp3"
      volume_size           = 20
      encrypted             = true
      delete_on_termination = true
    }
  }
}

resource "aws_eks_node_group" "lab" {
  cluster_name    = aws_eks_cluster.lab.name
  node_group_name = "${var.name}-workers"
  node_role_arn   = aws_iam_role.nodes.arn
  subnet_ids      = [for subnet in aws_subnet.private : subnet.id]
  version         = var.kubernetes_version
  ami_type        = "AL2023_x86_64_STANDARD"
  capacity_type   = "ON_DEMAND"
  instance_types  = ["t3.medium"]

  scaling_config {
    min_size     = 1
    desired_size = 1
    max_size     = 2
  }
  update_config {
    max_unavailable = 1
  }
  launch_template {
    id      = aws_launch_template.nodes.id
    version = tostring(aws_launch_template.nodes.latest_version)
  }
  depends_on = [
    # Install both host-network DaemonSets and the CNI identity before compute.
    # Waiting for nodes before installing the agent would strand CNI credentials.
    aws_eks_addon.pod_identity_agent,
    aws_eks_addon.vpc_cni,
    aws_iam_role_policy_attachment.nodes,
    aws_route.private_egress,
    aws_route_table_association.private,
    aws_vpc_security_group_ingress_rule.api,
    aws_vpc_security_group_ingress_rule.node_mesh,
    aws_vpc_security_group_ingress_rule.control_to_nodes,
    aws_vpc_security_group_egress_rule.node_mesh,
    aws_vpc_security_group_egress_rule.node_https,
    aws_vpc_security_group_egress_rule.control_to_nodes,
  ]
}
