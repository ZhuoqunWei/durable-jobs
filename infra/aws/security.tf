resource "aws_security_group" "control_plane" {
  name_prefix = "${var.name}-control-"
  description = "Additional control-plane ENI rules; AWS also creates its primary cluster group"
  vpc_id      = aws_vpc.lab.id
}

resource "aws_security_group" "nodes" {
  name_prefix = "${var.name}-nodes-"
  description = "Explicit node group; no SSH or public inbound rules"
  vpc_id      = aws_vpc.lab.id
}

resource "aws_security_group" "operator" {
  name_prefix = "${var.name}-operator-"
  description = "For a future authorized in-VPC runner; no runner or inbound access is created"
  vpc_id      = aws_vpc.lab.id
}

resource "aws_vpc_security_group_ingress_rule" "api" {
  for_each = {
    nodes    = aws_security_group.nodes.id
    operator = aws_security_group.operator.id
  }
  security_group_id            = aws_security_group.control_plane.id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

resource "aws_vpc_security_group_egress_rule" "operator_api" {
  security_group_id            = aws_security_group.operator.id
  referenced_security_group_id = aws_security_group.control_plane.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}

# The CNI uses node addresses for pods. Allow same-group pod/node traffic,
# including DNS, explicitly. This is NOT per-pod or namespace isolation.
resource "aws_vpc_security_group_ingress_rule" "node_mesh" {
  security_group_id            = aws_security_group.nodes.id
  referenced_security_group_id = aws_security_group.nodes.id
  ip_protocol                  = "-1"
}

resource "aws_vpc_security_group_egress_rule" "node_mesh" {
  security_group_id            = aws_security_group.nodes.id
  referenced_security_group_id = aws_security_group.nodes.id
  ip_protocol                  = "-1"
}

resource "aws_vpc_security_group_ingress_rule" "control_to_nodes" {
  for_each                     = { kubelet = 10250, alb_webhook = 9443 }
  security_group_id            = aws_security_group.nodes.id
  referenced_security_group_id = aws_security_group.control_plane.id
  ip_protocol                  = "tcp"
  from_port                    = each.value
  to_port                      = each.value
}

resource "aws_vpc_security_group_egress_rule" "control_to_nodes" {
  for_each                     = { kubelet = 10250, alb_webhook = 9443 }
  security_group_id            = aws_security_group.control_plane.id
  referenced_security_group_id = aws_security_group.nodes.id
  ip_protocol                  = "tcp"
  from_port                    = each.value
  to_port                      = each.value
}

# Broad HTTPS destinations are a documented lab compromise, not a domain allowlist.
# The VPC DNS resolver is not filtered by security groups.
resource "aws_vpc_security_group_egress_rule" "node_https" {
  security_group_id = aws_security_group.nodes.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}
