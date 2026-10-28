# No real AWS provider instance is used. Every run is a mocked plan, never apply.
mock_provider "aws" {
  override_during = plan
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "111122223333"
      arn        = "arn:aws:iam::111122223333:role/offline-test"
    }
  }
  mock_resource "aws_eks_cluster" {
    override_during = plan
    defaults = {
      arn      = "arn:aws:eks:us-west-2:111122223333:cluster/durable-jobs-lab"
      endpoint = "https://offline-test.invalid"
    }
  }
  mock_resource "aws_launch_template" {
    override_during = plan
    defaults        = { id = "lt-00000000000000001", latest_version = 1 }
  }
  mock_resource "aws_vpc" {
    override_during = plan
    defaults        = { id = "vpc-00000000000000001" }
  }
  mock_resource "aws_internet_gateway" {
    override_during = plan
    defaults        = { id = "igw-00000000000000001" }
  }
  mock_resource "aws_route53_zone" {
    override_during = plan
    defaults        = { zone_id = "ZOFFLINEPRIVATE" }
  }
}

run "private_operator_and_run_cleanup_tags" {
  command = plan
  variables {
    operator_ami_id = "ami-00000000000000001"
    lab_run_id      = "lab-20260928T220000Z"
  }
  assert {
    condition = (
      aws_instance.operator[0].associate_public_ip_address == false &&
      aws_instance.operator[0].subnet_id == aws_subnet.private["us-west-2a"].id &&
      aws_instance.operator[0].metadata_options[0].http_tokens == "required" &&
      one(aws_instance.operator[0].root_block_device).encrypted &&
      one(aws_instance.operator[0].root_block_device).delete_on_termination &&
      aws_instance.operator[0].tags["LabRun"] == var.lab_run_id &&
      aws_instance.operator[0].volume_tags["LabRun"] == var.lab_run_id
    )
    error_message = "Private operator requires encrypted disposable disk, IMDSv2 and run ownership tags."
  }
  assert {
    condition = alltrue([
      for tags in aws_launch_template.nodes.tag_specifications : tags.tags["Project"] == var.name && tags.tags["LabRun"] == var.lab_run_id
    ]) && jsondecode(aws_eks_addon.ebs_csi.configuration_values).controller.extraVolumeTags.LabRun == var.lab_run_id
    error_message = "Managed-node disks and dynamically provisioned DB volumes must be discoverable by exact cleanup run."
  }
  assert {
    condition     = !contains(keys(jsondecode(aws_eks_addon.ebs_csi.configuration_values).controller), "env")
    error_message = "Leave managed EBS environment variables to EKS; duplicate AWS_REGION prevents installation."
  }
}

# Give related resources distinct IDs so swapped route/SG references fail tests.
override_data {
  target          = data.aws_lb.published[0]
  override_during = plan
  values = {
    internal           = true
    load_balancer_type = "application"
    vpc_id             = "vpc-00000000000000001"
    subnets            = ["subnet-00000000000000003", "subnet-00000000000000004"]
    security_groups    = ["sg-00000000000000005"]
    dns_name           = "internal-offline-fixture.us-west-2.elb.amazonaws.com"
    zone_id            = "ZOFFLINEALB"
    tags = {
      "elbv2.k8s.aws/cluster" = "durable-jobs-lab"
      "ingress.k8s.aws/stack" = "durable-jobs-aws/api"
    }
  }
}
override_resource {
  target          = aws_iam_role.cluster
  override_during = plan
  values          = { arn = "arn:aws:iam::111122223333:role/durable-jobs-lab-cluster" }
}
override_resource {
  target          = aws_iam_role.nodes
  override_during = plan
  values          = { arn = "arn:aws:iam::111122223333:role/durable-jobs-lab-nodes" }
}
override_resource {
  target          = aws_iam_role.ebs_csi
  override_during = plan
  values          = { arn = "arn:aws:iam::111122223333:role/durable-jobs-lab-ebs-csi" }
}
override_resource {
  target          = aws_iam_role.vpc_cni
  override_during = plan
  values          = { arn = "arn:aws:iam::111122223333:role/durable-jobs-lab-vpc-cni" }
}
override_resource {
  target          = aws_security_group.eks_auth
  override_during = plan
  values          = { id = "sg-00000000000000004" }
}
override_resource {
  target          = aws_security_group.alb
  override_during = plan
  values          = { id = "sg-00000000000000005" }
}
override_resource {
  target          = aws_iam_role.alb_controller
  override_during = plan
  values          = { arn = "arn:aws:iam::111122223333:role/durable-jobs-lab-alb-controller" }
}
override_resource {
  target          = aws_security_group.control_plane
  override_during = plan
  values          = { id = "sg-00000000000000001" }
}
override_resource {
  target          = aws_security_group.nodes
  override_during = plan
  values          = { id = "sg-00000000000000002" }
}
override_resource {
  target          = aws_security_group.operator
  override_during = plan
  values          = { id = "sg-00000000000000003" }
}
override_resource {
  target          = aws_subnet.public["us-west-2a"]
  override_during = plan
  values          = { id = "subnet-00000000000000001" }
}
override_resource {
  target          = aws_subnet.public["us-west-2b"]
  override_during = plan
  values          = { id = "subnet-00000000000000002" }
}
override_resource {
  target          = aws_subnet.private["us-west-2a"]
  override_during = plan
  values          = { id = "subnet-00000000000000003" }
}
override_resource {
  target          = aws_subnet.private["us-west-2b"]
  override_during = plan
  values          = { id = "subnet-00000000000000004" }
}
override_resource {
  target          = aws_nat_gateway.lab["us-west-2a"]
  override_during = plan
  values          = { id = "nat-00000000000000001" }
}
override_resource {
  target          = aws_route_table.public
  override_during = plan
  values          = { id = "rtb-00000000000000001" }
}
override_resource {
  target          = aws_route_table.private["us-west-2a"]
  override_during = plan
  values          = { id = "rtb-00000000000000002" }
}
override_resource {
  target          = aws_route_table.private["us-west-2b"]
  override_during = plan
  values          = { id = "rtb-00000000000000003" }
}

variables {
  deployment_approved = true
  expected_account_id = "111122223333"
  operator_role_arn   = "arn:aws:iam::111122223333:role/lab-operator"
  region              = "us-west-2"
  availability_zones  = ["us-west-2a", "us-west-2b"]
  # Synthetic shape-only fixtures. These are NOT available/compatible builds.
  addon_versions = {
    pod_identity_agent = "v0.0.0-eksbuild.1"
    ebs_csi            = "v0.0.0-eksbuild.2"
    vpc_cni            = "v0.0.0-eksbuild.3"
    kube_proxy         = "v0.0.0-eksbuild.4"
    coredns            = "v0.0.0-eksbuild.5"
  }
}

run "private_network_single_nat" {
  command = plan
  assert {
    condition = (
      aws_vpc.lab.enable_dns_support && aws_vpc.lab.enable_dns_hostnames &&
      aws_subnet.public["us-west-2a"].cidr_block == "10.42.0.0/24" &&
      aws_subnet.public["us-west-2b"].cidr_block == "10.42.1.0/24" &&
      aws_subnet.private["us-west-2a"].cidr_block == "10.42.16.0/20" &&
      aws_subnet.private["us-west-2b"].cidr_block == "10.42.32.0/20"
    )
    error_message = "DNS must work and the four subnet CIDRs must remain disjoint."
  }
  assert {
    condition = alltrue([
      for subnet in concat(values(aws_subnet.public), values(aws_subnet.private)) : !subnet.map_public_ip_on_launch
    ])
    error_message = "Instances must not acquire public addresses automatically."
  }
  assert {
    condition = length(aws_nat_gateway.lab) == 1 && alltrue([
      for route in aws_route.private_egress : route.nat_gateway_id == "nat-00000000000000001" && route.destination_cidr_block == "0.0.0.0/0"
    ])
    error_message = "Single-NAT mode must route both private subnets through the selected NAT, not an internet gateway."
  }
  assert {
    condition = alltrue([
      for az, association in aws_route_table_association.private :
      association.subnet_id == aws_subnet.private[az].id && association.route_table_id == aws_route_table.private[az].id
    ]) && aws_nat_gateway.lab["us-west-2a"].subnet_id == aws_subnet.public["us-west-2a"].id
    error_message = "Private route tables must be associated, and NAT must sit in a public subnet."
  }
  assert {
    condition = (
      aws_route.public_internet.gateway_id == aws_internet_gateway.lab.id &&
      alltrue([for association in aws_route_table_association.public : association.route_table_id == aws_route_table.public.id]) &&
      alltrue([for subnet in aws_subnet.private : subnet.tags["kubernetes.io/role/internal-elb"] == "1"]) &&
      alltrue([for subnet in aws_subnet.public : !contains(keys(subnet.tags), "kubernetes.io/role/elb")])
    )
    error_message = "Public NAT routing and private-only load-balancer discovery must be explicit."
  }
}

run "per_az_egress" {
  command = plan
  variables { nat_mode = "per_az" }
  override_resource {
    target          = aws_nat_gateway.lab["us-west-2b"]
    override_during = plan
    values          = { id = "nat-00000000000000002" }
  }
  assert {
    condition = (
      length(aws_nat_gateway.lab) == 2 &&
      aws_route.private_egress["us-west-2a"].nat_gateway_id == "nat-00000000000000001" &&
      aws_route.private_egress["us-west-2b"].nat_gateway_id == "nat-00000000000000002" &&
      alltrue([for az, nat in aws_nat_gateway.lab : nat.subnet_id == aws_subnet.public[az].id])
    )
    error_message = "Per-AZ mode must use distinct NAT gateways in their respective AZs."
  }
}

run "private_cluster_and_bounded_nodes" {
  command = plan
  assert {
    condition = (
      aws_eks_cluster.lab.vpc_config[0].endpoint_private_access &&
      !aws_eks_cluster.lab.vpc_config[0].endpoint_public_access &&
      toset(aws_eks_cluster.lab.vpc_config[0].subnet_ids) == toset(["subnet-00000000000000003", "subnet-00000000000000004"]) &&
      toset(aws_eks_node_group.lab.subnet_ids) == toset(["subnet-00000000000000003", "subnet-00000000000000004"])
    )
    error_message = "The cluster endpoint and all worker machines must stay private."
  }
  assert {
    condition = (
      aws_eks_node_group.lab.scaling_config[0].min_size == 1 &&
      aws_eks_node_group.lab.scaling_config[0].desired_size == 1 &&
      aws_eks_node_group.lab.scaling_config[0].max_size == 2 &&
      aws_eks_node_group.lab.ami_type == "AL2023_x86_64_STANDARD" &&
      aws_eks_cluster.lab.upgrade_policy[0].support_type == "STANDARD"
    )
    error_message = "Keep the small lab size and reviewed OS/support mode; this is not a highly available workload."
  }
  assert {
    condition = (
      aws_launch_template.nodes.metadata_options[0].http_tokens == "required" &&
      aws_launch_template.nodes.metadata_options[0].http_put_response_hop_limit == 1 &&
      one(aws_launch_template.nodes.block_device_mappings).ebs[0].encrypted &&
      toset(aws_launch_template.nodes.vpc_security_group_ids) == toset(["sg-00000000000000002"])
    )
    error_message = "Nodes require IMDSv2, encrypted root storage and only the explicit node security group."
  }
}

run "security_group_paths" {
  command = plan
  assert {
    condition = alltrue([
      for key, rule in aws_vpc_security_group_ingress_rule.api :
      rule.security_group_id == "sg-00000000000000001" && rule.ip_protocol == "tcp" &&
      rule.from_port == 443 && rule.to_port == 443 &&
      rule.referenced_security_group_id == (key == "nodes" ? "sg-00000000000000002" : "sg-00000000000000003") &&
      rule.cidr_ipv4 == null && rule.cidr_ipv6 == null
    ])
    error_message = "API access must reference node/operator groups, not public CIDRs."
  }
  assert {
    condition = alltrue([
      for key, rule in aws_vpc_security_group_ingress_rule.control_to_nodes :
      rule.security_group_id == "sg-00000000000000002" && rule.referenced_security_group_id == "sg-00000000000000001" &&
      rule.from_port == (key == "kubelet" ? 10250 : 9443) && rule.from_port == rule.to_port &&
      rule.cidr_ipv4 == null && rule.cidr_ipv6 == null
    ]) && aws_vpc_security_group_ingress_rule.node_mesh.referenced_security_group_id == "sg-00000000000000002"
    error_message = "Nodes accept only the configured control-plane ports and their own group."
  }
  assert {
    condition = (
      aws_vpc_security_group_egress_rule.operator_api.referenced_security_group_id == "sg-00000000000000001" &&
      aws_vpc_security_group_egress_rule.operator_api.from_port == 443 &&
      aws_vpc_security_group_egress_rule.node_https.cidr_ipv4 == "0.0.0.0/0" &&
      aws_vpc_security_group_egress_rule.node_https.from_port == 443 &&
      aws_vpc_security_group_egress_rule.node_https.to_port == 443
    )
    error_message = "Operator access and the documented broad HTTPS egress compromise must stay explicit."
  }
}

run "explicit_access_logs_and_private_dns" {
  command = plan
  assert {
    condition = (
      aws_eks_cluster.lab.access_config[0].authentication_mode == "API" &&
      !aws_eks_cluster.lab.access_config[0].bootstrap_cluster_creator_admin_permissions &&
      aws_eks_access_entry.operator.principal_arn == var.operator_role_arn &&
      aws_eks_access_policy_association.operator.policy_arn == "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy" &&
      aws_eks_access_policy_association.operator.access_scope[0].type == "cluster"
    )
    error_message = "Administration must go to the specifically reviewed role, not implicitly to the creator."
  }
  assert {
    condition = (
      aws_cloudwatch_log_group.control_plane.retention_in_days == 7 &&
      contains(aws_eks_cluster.lab.enabled_cluster_log_types, "audit") &&
      one(aws_route53_zone.lab.vpc).vpc_id == aws_vpc.lab.id &&
      aws_route53_zone.lab.name == "durable-jobs-lab.internal"
    )
    error_message = "Keep bounded audit-log retention and a VPC-bound private DNS zone."
  }
}

run "storage_controller_identity_boundary" {
  command = plan
  assert {
    condition = jsondecode(aws_iam_role.ebs_csi.assume_role_policy) == jsondecode(jsonencode({
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Principal = { Service = "pods.eks.amazonaws.com" }
        Action    = ["sts:AssumeRole", "sts:TagSession"]
        Condition = {
          StringEquals = {
            "aws:RequestTag/eks-cluster-arn"            = "arn:aws:eks:us-west-2:111122223333:cluster/durable-jobs-lab"
            "aws:RequestTag/kubernetes-namespace"       = "kube-system"
            "aws:RequestTag/kubernetes-service-account" = "ebs-csi-controller-sa"
          }
        }
      }]
    }))
    error_message = "Only this cluster's kube-system EBS controller may assume the role, with session tags enabled."
  }
  assert {
    condition = (
      aws_iam_role_policy_attachment.ebs_csi.role == aws_iam_role.ebs_csi.name &&
      aws_iam_role_policy_attachment.ebs_csi.policy_arn == "arn:aws:iam::aws:policy/AmazonEBSCSIDriverPolicyV2" &&
      aws_iam_role.ebs_csi.name != aws_iam_role.nodes.name &&
      toset(keys(aws_iam_role_policy_attachment.nodes)) == toset([
        "AmazonEKSWorkerNodePolicy", "AmazonEC2ContainerRegistryReadOnly"
        ]) && alltrue([
        for name, attachment in aws_iam_role_policy_attachment.nodes :
        attachment.role == aws_iam_role.nodes.name && attachment.policy_arn == "arn:aws:iam::aws:policy/${name}"
      ])
    )
    error_message = "Use the canonical V2 policy on only the dedicated EBS role; nodes must have neither storage/admin nor CNI permissions."
  }
}

run "pinned_addons_and_owned_association" {
  command = plan
  assert {
    condition = (
      aws_eks_addon.pod_identity_agent.cluster_name == aws_eks_cluster.lab.name &&
      aws_eks_addon.ebs_csi.cluster_name == aws_eks_cluster.lab.name &&
      aws_eks_addon.pod_identity_agent.addon_name == "eks-pod-identity-agent" &&
      aws_eks_addon.ebs_csi.addon_name == "aws-ebs-csi-driver" &&
      aws_eks_addon.pod_identity_agent.addon_version == var.addon_versions.pod_identity_agent &&
      aws_eks_addon.ebs_csi.addon_version == var.addon_versions.ebs_csi &&
      aws_eks_addon.pod_identity_agent.resolve_conflicts_on_create == "NONE" &&
      aws_eks_addon.pod_identity_agent.resolve_conflicts_on_update == "NONE" &&
      aws_eks_addon.ebs_csi.resolve_conflicts_on_create == "NONE" &&
      aws_eks_addon.ebs_csi.resolve_conflicts_on_update == "NONE"
    )
    error_message = "Use explicit add-on pins and fail on conflicting existing installations rather than overwriting them."
  }
  assert {
    condition = (
      length(aws_eks_addon.ebs_csi.pod_identity_association) == 1 &&
      one(aws_eks_addon.ebs_csi.pod_identity_association).role_arn == "arn:aws:iam::111122223333:role/durable-jobs-lab-ebs-csi" &&
      one(aws_eks_addon.ebs_csi.pod_identity_association).service_account == "ebs-csi-controller-sa" &&
      aws_eks_addon.ebs_csi.service_account_role_arn == null &&
      aws_eks_addon.pod_identity_agent.service_account_role_arn == null
    )
    error_message = "The EBS add-on must own exactly one controller association, without mixing in an IRSA role."
  }
}

run "private_pod_identity_credential_path" {
  command = plan
  assert {
    condition = (
      aws_vpc_endpoint.eks_auth.vpc_id == "vpc-00000000000000001" &&
      aws_vpc_endpoint.eks_auth.vpc_endpoint_type == "Interface" &&
      aws_vpc_endpoint.eks_auth.service_name == "com.amazonaws.us-west-2.eks-auth" &&
      aws_vpc_endpoint.eks_auth.private_dns_enabled &&
      toset(aws_vpc_endpoint.eks_auth.subnet_ids) == toset(["subnet-00000000000000003", "subnet-00000000000000004"]) &&
      toset(aws_vpc_endpoint.eks_auth.security_group_ids) == toset(["sg-00000000000000004"])
    )
    error_message = "EKS Auth must use private DNS and an interface endpoint in the two private subnets."
  }
  assert {
    condition = (
      aws_security_group.eks_auth.vpc_id == "vpc-00000000000000001" &&
      aws_vpc_security_group_ingress_rule.eks_auth.security_group_id == "sg-00000000000000004" &&
      aws_vpc_security_group_ingress_rule.eks_auth.referenced_security_group_id == "sg-00000000000000002" &&
      aws_vpc_security_group_ingress_rule.eks_auth.ip_protocol == "tcp" &&
      aws_vpc_security_group_ingress_rule.eks_auth.from_port == 443 &&
      aws_vpc_security_group_ingress_rule.eks_auth.to_port == 443 &&
      aws_vpc_security_group_ingress_rule.eks_auth.cidr_ipv4 == null &&
      aws_vpc_security_group_ingress_rule.eks_auth.cidr_ipv6 == null
    )
    error_message = "Only lab nodes may initiate HTTPS to the credential endpoint; no public CIDR ingress."
  }
  assert {
    condition = jsondecode(aws_vpc_endpoint.eks_auth.policy) == jsondecode(jsonencode({
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Principal = "*"
        Action    = "eks-auth:AssumeRoleForPodIdentity"
        Resource  = "*"
        Condition = { StringEquals = { "aws:PrincipalArn" = "arn:aws:iam::111122223333:role/durable-jobs-lab-nodes" } }
      }]
    }))
    error_message = "The endpoint policy must admit only the node role and the Pod Identity credential action."
  }
}

run "refuse_missing_addon_pins" {
  command = plan
  variables {
    addon_versions = { pod_identity_agent = "", ebs_csi = "", vpc_cni = "", kube_proxy = "", coredns = "" }
  }
  expect_failures = [var.addon_versions]
}

run "refuse_mutable_agent_version" {
  command = plan
  variables {
    addon_versions = {
      pod_identity_agent = "latest", ebs_csi = "v0.0.0-eksbuild.2",
      vpc_cni            = "v0.0.0-eksbuild.3", kube_proxy = "v0.0.0-eksbuild.4", coredns = "v0.0.0-eksbuild.5"
    }
  }
  expect_failures = [var.addon_versions]
}

run "refuse_unpinned_driver_build" {
  command = plan
  variables {
    addon_versions = {
      pod_identity_agent = "v0.0.0-eksbuild.1", ebs_csi = "v1.2.3",
      vpc_cni            = "v0.0.0-eksbuild.3", kube_proxy = "v0.0.0-eksbuild.4", coredns = "v0.0.0-eksbuild.5"
    }
  }
  expect_failures = [var.addon_versions]
}

run "refuse_partial_addon_selection" {
  command = plan
  variables {
    addon_versions = {
      pod_identity_agent = "v0.0.0-eksbuild.1", ebs_csi = "",
      vpc_cni            = "v0.0.0-eksbuild.3", kube_proxy = "v0.0.0-eksbuild.4", coredns = "v0.0.0-eksbuild.5"
    }
  }
  expect_failures = [var.addon_versions]
}

run "refuse_null_addon_version" {
  command = plan
  variables {
    addon_versions = {
      pod_identity_agent = null, ebs_csi = "v0.0.0-eksbuild.2",
      vpc_cni            = "v0.0.0-eksbuild.3", kube_proxy = "v0.0.0-eksbuild.4", coredns = "v0.0.0-eksbuild.5"
    }
  }
  expect_failures = [var.addon_versions]
}

run "refuse_unapproved_deployment" {
  command = plan
  variables { deployment_approved = false }
  expect_failures = [terraform_data.deployment_gate]
}

run "refuse_wrong_account" {
  command = plan
  override_data {
    target          = data.aws_caller_identity.current
    override_during = plan
    values          = { account_id = "999988887777" }
  }
  expect_failures = [terraform_data.deployment_gate]
}

run "refuse_foreign_operator_role" {
  command = plan
  variables { operator_role_arn = "arn:aws:iam::999988887777:role/lab-operator" }
  expect_failures = [var.operator_role_arn]
}

run "refuse_root_operator" {
  command = plan
  variables { operator_role_arn = "arn:aws:iam::111122223333:root" }
  expect_failures = [var.operator_role_arn]
}

run "refuse_duplicate_zones" {
  command = plan
  variables { availability_zones = ["us-west-2a", "us-west-2a"] }
  expect_failures = [var.availability_zones]
}

run "refuse_public_vpc_cidr" {
  command = plan
  variables { vpc_cidr = "8.8.0.0/16" }
  expect_failures = [var.vpc_cidr]
}

run "refuse_unknown_nat_mode" {
  command = plan
  variables { nat_mode = "disabled" }
  expect_failures = [var.nat_mode]
}

run "alb_identity_and_iam_boundaries" {
  command = plan
  assert {
    condition = (
      filesha256("reference/alb-iam-v3.5.0.json") == "16f232c9d9f79366fe949c4550ad517a202380058a9e48d45a4e215044a20a6a" &&
      aws_eks_pod_identity_association.alb_controller.cluster_name == aws_eks_cluster.lab.name &&
      aws_eks_pod_identity_association.alb_controller.namespace == "kube-system" &&
      aws_eks_pod_identity_association.alb_controller.service_account == "aws-load-balancer-controller" &&
      aws_eks_pod_identity_association.alb_controller.role_arn == aws_iam_role.alb_controller.arn &&
      aws_iam_role_policy.alb_controller.role == aws_iam_role.alb_controller.id
    )
    error_message = "Controller policy reference and its own Pod Identity association must remain pinned and separate."
  }
  assert {
    condition = jsondecode(aws_iam_role.alb_controller.assume_role_policy) == jsondecode(jsonencode({
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Principal = { Service = "pods.eks.amazonaws.com" }
        Action    = ["sts:AssumeRole", "sts:TagSession"]
        Condition = { StringEquals = {
          "aws:RequestTag/eks-cluster-arn"            = "arn:aws:eks:us-west-2:111122223333:cluster/durable-jobs-lab"
          "aws:RequestTag/kubernetes-namespace"       = "kube-system"
          "aws:RequestTag/kubernetes-service-account" = "aws-load-balancer-controller"
        } }
      }]
    }))
    error_message = "Controller trust must match this cluster, namespace and service account exactly."
  }
  assert {
    condition = alltrue(flatten([
      for statement in jsondecode(aws_iam_role_policy.alb_controller.policy).Statement : [
        for action in statement.Action : (
          startswith(action, "elasticloadbalancing:") || startswith(action, "ec2:Describe") ||
          startswith(action, "ec2:Get") || contains(["acm:ListCertificates", "acm:DescribeCertificate"], action)
        ) && !contains(["elasticloadbalancing:SetWebAcl", "elasticloadbalancing:ModifyCapacityReservation", "elasticloadbalancing:ModifyIpPools"], action)
      ] if statement.Effect == "Allow"
    ])) && length(aws_iam_role_policy.alb_controller.policy) < 10240
    error_message = "Do not grant SG mutations, IAM, DNS, WAF, Shield or Cognito; keep the inline role policy within its quota."
  }
  assert {
    condition = alltrue([
      for statement in local.alb_statements : statement.Condition.StringEquals["aws:RequestedRegion"] == "us-west-2" && alltrue([
        for key, value in try(statement.Condition.Null, {}) : statement.Condition.StringEquals[key] == "durable-jobs-lab" if value == "false"
      ]) && alltrue([for arn in statement.Resource : arn == "*" || startswith(arn, "arn:aws:elasticloadbalancing:us-west-2:111122223333:")])
      ]) && alltrue([
      for statement in local.alb_statements : !contains(statement.Resource, "*")
      if anytrue([for action in statement.Action : startswith(action, "elasticloadbalancing:") && !startswith(action, "elasticloadbalancing:Describe")])
    ])
    error_message = "ELB writes stay in this account/region, and cluster-tagged operations require the exact cluster tag."
  }
  assert {
    condition = one([
      for statement in jsondecode(aws_iam_role_policy.alb_controller.policy).Statement : statement
      if statement.Effect == "Deny"
      ]) == jsondecode(jsonencode({
        Sid       = "DenyInternetFacingCreation", Effect = "Deny", Action = ["elasticloadbalancing:CreateLoadBalancer"], Resource = ["*"],
        Condition = { StringNotEquals = { "elasticloadbalancing:Scheme" = "internal" } }
    }))
    error_message = "Public ALB creation must be explicitly denied, not only discouraged by a manifest default."
  }
}

run "alb_network_and_unpublished_default" {
  command = plan
  assert {
    condition = (
      aws_vpc_security_group_ingress_rule.operator_to_alb.security_group_id == "sg-00000000000000005" &&
      aws_vpc_security_group_ingress_rule.operator_to_alb.referenced_security_group_id == "sg-00000000000000003" &&
      aws_vpc_security_group_egress_rule.operator_to_alb.security_group_id == "sg-00000000000000003" &&
      aws_vpc_security_group_egress_rule.operator_to_alb.referenced_security_group_id == "sg-00000000000000005" &&
      aws_vpc_security_group_ingress_rule.alb_to_api.security_group_id == "sg-00000000000000002" &&
      aws_vpc_security_group_ingress_rule.alb_to_api.referenced_security_group_id == "sg-00000000000000005" &&
      aws_vpc_security_group_egress_rule.alb_to_api.security_group_id == "sg-00000000000000005" &&
      aws_vpc_security_group_egress_rule.alb_to_api.referenced_security_group_id == "sg-00000000000000002"
    )
    error_message = "Only the operator reaches the ALB; the ALB reaches the node/pod group, not a public CIDR."
  }
  assert {
    condition = alltrue([
      for rule in [aws_vpc_security_group_ingress_rule.operator_to_alb, aws_vpc_security_group_egress_rule.operator_to_alb] :
      rule.from_port == 443 && rule.to_port == 443 && rule.ip_protocol == "tcp" && rule.cidr_ipv4 == null && rule.cidr_ipv6 == null
      ]) && alltrue([
      for rule in [aws_vpc_security_group_ingress_rule.alb_to_api, aws_vpc_security_group_egress_rule.alb_to_api] :
      rule.from_port == 8085 && rule.to_port == 8085 && rule.ip_protocol == "tcp" && rule.cidr_ipv4 == null && rule.cidr_ipv6 == null
    ]) && length(data.aws_lb.published) == 0 && length(aws_route53_record.api) == 0
    error_message = "Keep HTTPS-only frontend, API-only backend ports, and no ALB lookup or DNS publication on the initial plan."
  }
}

run "publish_only_verified_private_alb" {
  command = plan
  variables { published_alb_arn = "arn:aws:elasticloadbalancing:us-west-2:111122223333:loadbalancer/app/lab/0123456789abcdef" }
  assert {
    condition = (
      aws_route53_record.api[0].zone_id == aws_route53_zone.lab.zone_id &&
      aws_route53_record.api[0].name == "jobs.durable-jobs-lab.internal" &&
      aws_route53_record.api[0].type == "A" && !aws_route53_record.api[0].allow_overwrite &&
      one(aws_route53_record.api[0].alias).name == "internal-offline-fixture.us-west-2.elb.amazonaws.com" &&
      one(aws_route53_record.api[0].alias).zone_id == "ZOFFLINEALB" &&
      one(aws_route53_record.api[0].alias).evaluate_target_health
    )
    error_message = "The private A alias must use the controller-created ALB's actual DNS and canonical zone, without overwriting existing records."
  }
}

run "refuse_public_alb_alias" {
  command = plan
  variables { published_alb_arn = "arn:aws:elasticloadbalancing:us-west-2:111122223333:loadbalancer/app/lab/0123456789abcdef" }
  override_data {
    target          = data.aws_lb.published[0]
    override_during = plan
    values = {
      internal           = false
      load_balancer_type = "application"
      vpc_id             = "vpc-00000000000000001"
      subnets            = ["subnet-00000000000000003", "subnet-00000000000000004"]
      security_groups    = ["sg-00000000000000005"]
      dns_name           = "internal-offline-fixture.us-west-2.elb.amazonaws.com"
      zone_id            = "ZOFFLINEALB"
      tags = {
        "elbv2.k8s.aws/cluster" = "durable-jobs-lab"
        "ingress.k8s.aws/stack" = "durable-jobs-aws/api"
      }
    }
  }
  expect_failures = [aws_route53_record.api[0]]
}

run "refuse_wrong_vpc_alias" {
  command = plan
  variables { published_alb_arn = "arn:aws:elasticloadbalancing:us-west-2:111122223333:loadbalancer/app/lab/0123456789abcdef" }
  override_data {
    target          = data.aws_lb.published[0]
    override_during = plan
    values = {
      internal           = true
      load_balancer_type = "application"
      vpc_id             = "vpc-00000000000000009"
      subnets            = ["subnet-00000000000000003", "subnet-00000000000000004"]
      security_groups    = ["sg-00000000000000005"]
      dns_name           = "internal-offline-fixture.us-west-2.elb.amazonaws.com"
      zone_id            = "ZOFFLINEALB"
      tags = {
        "elbv2.k8s.aws/cluster" = "durable-jobs-lab"
        "ingress.k8s.aws/stack" = "durable-jobs-aws/api"
      }
    }
  }
  expect_failures = [aws_route53_record.api[0]]
}

run "refuse_wrong_owner_alias" {
  command = plan
  variables { published_alb_arn = "arn:aws:elasticloadbalancing:us-west-2:111122223333:loadbalancer/app/lab/0123456789abcdef" }
  override_data {
    target          = data.aws_lb.published[0]
    override_during = plan
    values = {
      internal           = true
      load_balancer_type = "application"
      vpc_id             = "vpc-00000000000000001"
      subnets            = ["subnet-00000000000000003", "subnet-00000000000000004"]
      security_groups    = ["sg-00000000000000005"]
      dns_name           = "internal-offline-fixture.us-west-2.elb.amazonaws.com"
      zone_id            = "ZOFFLINEALB"
      tags = {
        "elbv2.k8s.aws/cluster" = "other"
        "ingress.k8s.aws/stack" = "durable-jobs-aws/api"
      }
    }
  }
  expect_failures = [aws_route53_record.api[0]]
}

run "refuse_public_subnets_alias" {
  command = plan
  variables { published_alb_arn = "arn:aws:elasticloadbalancing:us-west-2:111122223333:loadbalancer/app/lab/0123456789abcdef" }
  override_data {
    target          = data.aws_lb.published[0]
    override_during = plan
    values = {
      internal           = true
      load_balancer_type = "application"
      vpc_id             = "vpc-00000000000000001"
      subnets            = ["subnet-00000000000000001", "subnet-00000000000000002"]
      security_groups    = ["sg-00000000000000005"]
      dns_name           = "internal-offline-fixture.us-west-2.elb.amazonaws.com"
      zone_id            = "ZOFFLINEALB"
      tags = {
        "elbv2.k8s.aws/cluster" = "durable-jobs-lab"
        "ingress.k8s.aws/stack" = "durable-jobs-aws/api"
      }
    }
  }
  expect_failures = [aws_route53_record.api[0]]
}

run "refuse_unexpected_sg_alias" {
  command = plan
  variables { published_alb_arn = "arn:aws:elasticloadbalancing:us-west-2:111122223333:loadbalancer/app/lab/0123456789abcdef" }
  override_data {
    target          = data.aws_lb.published[0]
    override_during = plan
    values = {
      internal           = true
      load_balancer_type = "application"
      vpc_id             = "vpc-00000000000000001"
      subnets            = ["subnet-00000000000000003", "subnet-00000000000000004"]
      security_groups    = ["sg-00000000000000009"]
      dns_name           = "internal-offline-fixture.us-west-2.elb.amazonaws.com"
      zone_id            = "ZOFFLINEALB"
      tags = {
        "elbv2.k8s.aws/cluster" = "durable-jobs-lab"
        "ingress.k8s.aws/stack" = "durable-jobs-aws/api"
      }
    }
  }
  expect_failures = [aws_route53_record.api[0]]
}

run "refuse_foreign_alb_arn" {
  command = plan
  variables { published_alb_arn = "arn:aws:elasticloadbalancing:us-east-1:999988887777:loadbalancer/app/lab/0123456789abcdef" }
  expect_failures = [var.published_alb_arn]
}

run "refuse_other_ingress_alias" {
  command = plan
  variables { published_alb_arn = "arn:aws:elasticloadbalancing:us-west-2:111122223333:loadbalancer/app/lab/0123456789abcdef" }
  override_data {
    target          = data.aws_lb.published[0]
    override_during = plan
    values = {
      internal           = true
      load_balancer_type = "application"
      vpc_id             = "vpc-00000000000000001"
      subnets            = ["subnet-00000000000000003", "subnet-00000000000000004"]
      security_groups    = ["sg-00000000000000005"]
      dns_name           = "internal-offline-fixture.us-west-2.elb.amazonaws.com"
      zone_id            = "ZOFFLINEALB"
      tags = {
        "elbv2.k8s.aws/cluster" = "durable-jobs-lab"
        "ingress.k8s.aws/stack" = "durable-jobs-aws/other-service"
      }
    }
  }
  expect_failures = [aws_route53_record.api[0]]
}

run "refuse_invalid_hostname_label" {
  command = plan
  variables { name = "lab-" }
  expect_failures = [var.name]
}
