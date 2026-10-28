# Shares the same no-credential, plan-only boundary; fixture versions are fake.
mock_provider "aws" {
  override_during = plan
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111122223333" }
  }
  mock_resource "aws_eks_cluster" {
    override_during = plan
    defaults        = { arn = "arn:aws:eks:us-west-2:111122223333:cluster/durable-jobs-lab" }
  }
}
override_resource {
  target          = aws_iam_role.vpc_cni
  override_during = plan
  values          = { arn = "arn:aws:iam::111122223333:role/durable-jobs-lab-vpc-cni" }
}

variables {
  deployment_approved = true
  expected_account_id = "111122223333"
  operator_role_arn   = "arn:aws:iam::111122223333:role/lab-operator"
  region              = "us-west-2"
  availability_zones  = ["us-west-2a", "us-west-2b"]
  addon_versions = {
    pod_identity_agent = "v0.0.0-eksbuild.1"
    ebs_csi            = "v0.0.0-eksbuild.2"
    vpc_cni            = "v0.0.0-eksbuild.3"
    kube_proxy         = "v0.0.0-eksbuild.4"
    coredns            = "v0.0.0-eksbuild.5"
  }
}

run "cni_identity_boundary" {
  command = plan
  assert {
    condition = jsondecode(aws_iam_role.vpc_cni.assume_role_policy) == jsondecode(jsonencode({
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Principal = { Service = "pods.eks.amazonaws.com" }
        Action    = ["sts:AssumeRole", "sts:TagSession"]
        Condition = {
          StringEquals = {
            "aws:RequestTag/eks-cluster-arn"            = "arn:aws:eks:us-west-2:111122223333:cluster/durable-jobs-lab"
            "aws:RequestTag/kubernetes-namespace"       = "kube-system"
            "aws:RequestTag/kubernetes-service-account" = "aws-node"
          }
        }
      }]
    }))
    error_message = "Only aws-node in this cluster's kube-system may assume the CNI role."
  }
  assert {
    condition = (
      aws_iam_role_policy_attachment.vpc_cni.role == aws_iam_role.vpc_cni.name &&
      aws_iam_role_policy_attachment.vpc_cni.policy_arn == "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy" &&
      aws_iam_role.vpc_cni.name != aws_iam_role.nodes.name &&
      aws_iam_role.vpc_cni.name != aws_iam_role.ebs_csi.name &&
      aws_iam_role.vpc_cni.name != aws_iam_role.alb_controller.name &&
      toset(keys(aws_iam_role_policy_attachment.nodes)) == toset(["AmazonEKSWorkerNodePolicy", "AmazonEC2ContainerRegistryReadOnly"])
    )
    error_message = "CNI permissions belong to a dedicated role, not nodes or another controller."
  }
  assert {
    condition = (
      length(aws_eks_addon.vpc_cni.pod_identity_association) == 1 &&
      one(aws_eks_addon.vpc_cni.pod_identity_association).service_account == "aws-node" &&
      one(aws_eks_addon.vpc_cni.pod_identity_association).role_arn == "arn:aws:iam::111122223333:role/durable-jobs-lab-vpc-cni" &&
      aws_eks_addon.vpc_cni.service_account_role_arn == null
    )
    error_message = "The managed CNI add-on owns exactly one aws-node association, without IRSA."
  }
}

run "managed_network_addons_replace_bootstrap_defaults" {
  command = plan
  assert {
    condition     = !aws_eks_cluster.lab.bootstrap_self_managed_addons
    error_message = "Fresh clusters must not install unmanaged networking defaults alongside these managed add-ons."
  }
  assert {
    condition = (
      aws_eks_addon.vpc_cni.addon_name == "vpc-cni" && aws_eks_addon.vpc_cni.addon_version == var.addon_versions.vpc_cni &&
      aws_eks_addon.kube_proxy.addon_name == "kube-proxy" && aws_eks_addon.kube_proxy.addon_version == var.addon_versions.kube_proxy &&
      aws_eks_addon.coredns.addon_name == "coredns" && aws_eks_addon.coredns.addon_version == var.addon_versions.coredns &&
      alltrue([for addon in [aws_eks_addon.vpc_cni, aws_eks_addon.kube_proxy, aws_eks_addon.coredns] :
        addon.cluster_name == aws_eks_cluster.lab.name &&
        addon.resolve_conflicts_on_create == "NONE" && addon.resolve_conflicts_on_update == "NONE"
      ])
    )
    error_message = "All three managed network/DNS add-ons need exact pins and must refuse ownership conflicts."
  }
  assert {
    condition = (
      aws_eks_addon.coredns.service_account_role_arn == null && aws_eks_addon.kube_proxy.service_account_role_arn == null &&
      length(aws_eks_addon.coredns.pod_identity_association) == 0 && length(aws_eks_addon.kube_proxy.pod_identity_association) == 0
    )
    error_message = "DNS and Service routing must not receive unnecessary AWS roles."
  }
}

run "refuse_mutable_cni_version" {
  command = plan
  variables {
    addon_versions = {
      pod_identity_agent = "v0.0.0-eksbuild.1", ebs_csi = "v0.0.0-eksbuild.2",
      vpc_cni            = "latest", kube_proxy = "v0.0.0-eksbuild.4", coredns = "v0.0.0-eksbuild.5"
    }
  }
  expect_failures = [var.addon_versions]
}

run "refuse_missing_proxy_pin" {
  command = plan
  variables {
    addon_versions = {
      pod_identity_agent = "v0.0.0-eksbuild.1", ebs_csi = "v0.0.0-eksbuild.2",
      vpc_cni            = "v0.0.0-eksbuild.3", kube_proxy = "", coredns = "v0.0.0-eksbuild.5"
    }
  }
  expect_failures = [var.addon_versions]
}

run "refuse_null_dns_pin" {
  command = plan
  variables {
    addon_versions = {
      pod_identity_agent = "v0.0.0-eksbuild.1", ebs_csi = "v0.0.0-eksbuild.2",
      vpc_cni            = "v0.0.0-eksbuild.3", kube_proxy = "v0.0.0-eksbuild.4", coredns = null
    }
  }
  expect_failures = [var.addon_versions]
}
