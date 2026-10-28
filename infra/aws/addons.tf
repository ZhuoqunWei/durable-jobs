# The controller gets AWS credentials; the application service account does not.
# Keep Pod Identity's default session tags enabled: the trust conditions use them.
resource "aws_iam_role" "ebs_csi" {
  name = "${var.name}-ebs-csi"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = {
        StringEquals = {
          "aws:RequestTag/eks-cluster-arn"            = aws_eks_cluster.lab.arn
          "aws:RequestTag/kubernetes-namespace"       = "kube-system"
          "aws:RequestTag/kubernetes-service-account" = "ebs-csi-controller-sa"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role = aws_iam_role.ebs_csi.name
  # V2 uses the root policy path (not policy/service-role/). AWS manages its
  # contents: CSI-tag conditions are not isolation to this particular cluster.
  policy_arn = "arn:aws:iam::aws:policy/AmazonEBSCSIDriverPolicyV2"
}

resource "aws_eks_addon" "pod_identity_agent" {
  cluster_name                = aws_eks_cluster.lab.name
  addon_name                  = "eks-pod-identity-agent"
  addon_version               = var.addon_versions.pod_identity_agent
  resolve_conflicts_on_create = "NONE"
  resolve_conflicts_on_update = "NONE"

  # Prepare before compute. This is a host-network DaemonSet, not a Deployment
  # whose replicas must run before nodes exist. WorkerNodePolicy already allows
  # eks-auth:AssumeRoleForPodIdentity. Verify actual startup in the first AWS lab.
  depends_on = [
    aws_vpc_endpoint.eks_auth,
    aws_vpc_security_group_ingress_rule.eks_auth,
  ]
}

resource "aws_eks_addon" "ebs_csi" {
  cluster_name                = aws_eks_cluster.lab.name
  addon_name                  = "aws-ebs-csi-driver"
  addon_version               = var.addon_versions.ebs_csi
  resolve_conflicts_on_create = "NONE"
  resolve_conflicts_on_update = "NONE"
  configuration_values = jsonencode({
    controller = {
      extraVolumeTags = { Project = var.name, LabRun = var.lab_run_id }
      # The managed add-on injects AWS_REGION. Adding it to env again produces
      # duplicate merge keys and causes EKS to reject the Deployment.
    }
  })

  # The EKS add-on owns this association. Do not create a second standalone
  # association, a Helm installation, or an IRSA service_account_role_arn.
  pod_identity_association {
    role_arn        = aws_iam_role.ebs_csi.arn
    service_account = "ebs-csi-controller-sa"
  }

  depends_on = [
    aws_eks_node_group.lab,
    aws_eks_addon.coredns,
    aws_eks_addon.pod_identity_agent,
    aws_iam_role_policy_attachment.ebs_csi,
  ]
}
