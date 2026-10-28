locals {
  # Stable, AZ-keyed resource addresses. Public /24s do not overlap private /20s.
  zones     = { for index, az in var.availability_zones : az => index }
  nat_zones = var.nat_mode == "per_az" ? local.zones : { (var.availability_zones[0]) = 0 }
}

resource "aws_vpc" "lab" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = var.name }
  depends_on           = [terraform_data.deployment_gate]
}

resource "aws_subnet" "public" {
  for_each                = local.zones
  vpc_id                  = aws_vpc.lab.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, each.value)
  map_public_ip_on_launch = false
  # No internet-facing load-balancer discovery tag. This is not an IAM boundary.
  tags = { Name = "${var.name}-public-${each.key}" }
}

resource "aws_subnet" "private" {
  for_each                = local.zones
  vpc_id                  = aws_vpc.lab.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.vpc_cidr, 4, each.value + 1)
  map_public_ip_on_launch = false
  tags = {
    Name                              = "${var.name}-private-${each.key}"
    "kubernetes.io/role/internal-elb" = "1"
  }
}

resource "aws_internet_gateway" "lab" {
  vpc_id = aws_vpc.lab.id
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.lab.id
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.lab.id
}

resource "aws_route_table_association" "public" {
  for_each       = local.zones
  subnet_id      = aws_subnet.public[each.key].id
  route_table_id = aws_route_table.public.id
}

resource "aws_eip" "nat" {
  for_each   = local.nat_zones
  domain     = "vpc"
  depends_on = [terraform_data.deployment_gate]
}

resource "aws_nat_gateway" "lab" {
  for_each      = local.nat_zones
  allocation_id = aws_eip.nat[each.key].id
  subnet_id     = aws_subnet.public[each.key].id
  depends_on    = [aws_route.public_internet, aws_route_table_association.public]
}

resource "aws_route_table" "private" {
  for_each = local.zones
  vpc_id   = aws_vpc.lab.id
}

resource "aws_route" "private_egress" {
  for_each               = local.zones
  route_table_id         = aws_route_table.private[each.key].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.lab[var.nat_mode == "per_az" ? each.key : var.availability_zones[0]].id
}

resource "aws_route_table_association" "private" {
  for_each       = local.zones
  subnet_id      = aws_subnet.private[each.key].id
  route_table_id = aws_route_table.private[each.key].id
}

# Private name space. alb.tf owns the optional second-stage alias, not the ALB.
resource "aws_route53_zone" "lab" {
  name = "${var.name}.internal"
  vpc {
    vpc_id = aws_vpc.lab.id
  }
}
