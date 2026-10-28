# Fresh-cluster bootstrap, not an in-place migration of a running cluster.
# IPv4 only. Keep the Pod Identity session tags enabled for these trust checks.
resource "aws_iam_role" "vpc_cni" {
  name = "${var.name}-vpc-cni"
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
          "aws:RequestTag/kubernetes-service-account" = "aws-node"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "vpc_cni" {
  role       = aws_iam_role.vpc_cni.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
  # AWS manages this policy. Dedicated trust does not make its EC2 permissions
  # resource-scoped to this cluster; review the current policy before deployment.
}

resource "aws_eks_addon" "vpc_cni" {
  cluster_name                = aws_eks_cluster.lab.name
  addon_name                  = "vpc-cni"
  addon_version               = var.addon_versions.vpc_cni
  resolve_conflicts_on_create = "NONE"
  resolve_conflicts_on_update = "NONE"

  # EKS owns the association and injects Pod Identity credentials into aws-node.
  # No separate association, IRSA annotation or node-role fallback permission.
  pod_identity_association {
    role_arn        = aws_iam_role.vpc_cni.arn
    service_account = "aws-node"
  }
  depends_on = [aws_eks_addon.pod_identity_agent, aws_iam_role_policy_attachment.vpc_cni]
}

# Cluster creation no longer supplies unmanaged defaults. CoreDNS is a
# Deployment and needs compute; kube-proxy supplies Service routing first.
resource "aws_eks_addon" "kube_proxy" {
  cluster_name                = aws_eks_cluster.lab.name
  addon_name                  = "kube-proxy"
  addon_version               = var.addon_versions.kube_proxy
  resolve_conflicts_on_create = "NONE"
  resolve_conflicts_on_update = "NONE"
  depends_on                  = [aws_eks_node_group.lab]
}

resource "aws_eks_addon" "coredns" {
  cluster_name                = aws_eks_cluster.lab.name
  addon_name                  = "coredns"
  addon_version               = var.addon_versions.coredns
  resolve_conflicts_on_create = "NONE"
  resolve_conflicts_on_update = "NONE"
  depends_on                  = [aws_eks_node_group.lab, aws_eks_addon.kube_proxy]
}
