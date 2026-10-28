"""Independent, retryable cost-stop controller for one approved six-hour lab.

Runs outside the VPC so deleting NAT/nodes cannot remove its own access. Retains
IAM, state and logs; never deletes by name alone. No credentials in its input.
"""
from datetime import datetime, timedelta, timezone
import json
import os
import re


def owned(tags, config):
    if isinstance(tags, list):
        tags = {t["Key"]: t["Value"] for t in tags}
    return isinstance(tags, dict) and tags.get("Project") == config["cluster"] and tags.get("LabRun") == config["run_id"]


def validate(config, account, now):
    if config["account"] != account or not re.fullmatch(r"[0-9]{12}", account):
        raise ValueError("Wrong account")
    if config["region"] != "us-east-1" or config["cluster"] != "durable-jobs-lab":
        raise ValueError("Wrong region or project")
    if not re.fullmatch(r"lab-[0-9]{8}T[0-9]{6}Z", config["run_id"]):
        raise ValueError("Invalid run ID")
    start, cleanup, end = [datetime.fromisoformat(config[k]) for k in ("started_at", "cleanup_after", "deadline")]
    if any(t.tzinfo is None for t in (start, cleanup, end, now)):
        raise ValueError("Timezone required")
    if not start < cleanup < end or end - start > timedelta(hours=6) or end - cleanup < timedelta(hours=1):
        raise ValueError("Maximum six-hour session must reserve an hour for cleanup")
    return now >= cleanup


def missing(error, *codes):
    return getattr(error, "response", {}).get("Error", {}).get("Code") in codes


def all_pages(client, operation, key, **params):
    if client.can_paginate(operation):
        return [item for page in client.get_paginator(operation).paginate(**params) for item in page.get(key, [])]
    return getattr(client, operation)(**params).get(key, [])


def reconcile(config, clients, execute=False):
    ec2, eks, elb = clients["ec2"], clients["eks"], clients["elbv2"]
    report = {"mode": "cleanup" if execute else "inspect", "run_id": config["run_id"], "actions": [], "remaining": {}}
    filters = [{"Name": "tag:Project", "Values": [config["cluster"]]}, {"Name": "tag:LabRun", "Values": [config["run_id"]]}]

    def act(client, operation, **params):
        report["actions"].append({"operation": operation, "target": params})
        if execute:
            getattr(client, operation)(**params)

    try:
        cluster = eks.describe_cluster(name=config["cluster"])["cluster"]
    except Exception as error:
        if not missing(error, "ResourceNotFoundException"):
            raise
        cluster = None
    if cluster and not owned(cluster.get("tags"), config):
        raise ValueError("Cluster ownership mismatch; refusing all cleanup")
    groups = all_pages(eks, "list_nodegroups", "nodegroups", clusterName=config["cluster"]) if cluster else []
    report["remaining"]["nodegroups"] = len(groups)
    for name in groups:
        group = eks.describe_nodegroup(clusterName=config["cluster"], nodegroupName=name)["nodegroup"]
        if name != config["cluster"] + "-workers" or not owned(group.get("tags"), config):
            raise ValueError("Node group ownership mismatch")
        if group["status"] != "DELETING":
            act(eks, "delete_nodegroup", clusterName=config["cluster"], nodegroupName=name)
    # Stop the operator machine; managed nodes are removed via EKS, never by
    # terminating an autoscaled instance that would immediately be replaced.
    reservations = all_pages(ec2, "describe_instances", "Reservations", Filters=filters)
    live = [i for r in reservations for i in r["Instances"] if i["State"]["Name"] not in ("terminated", "shutting-down")]
    for instance in live:
        tags = {t["Key"]: t["Value"] for t in instance.get("Tags", [])}
        if tags.get("Role") == "lab-operator" and owned(tags, config):
            act(ec2, "terminate_instances", InstanceIds=[instance["InstanceId"]])
    report["remaining"]["instances"] = len(live)

    # Wait for controller pods to be gone before removing their external ALB.
    # Otherwise reconciliation could recreate it during teardown.
    load_balancers = []
    for lb in all_pages(elb, "describe_load_balancers", "LoadBalancers"):
        tags = elb.describe_tags(ResourceArns=[lb["LoadBalancerArn"]])["TagDescriptions"][0]["Tags"]
        if owned(tags, config):
            if lb["Scheme"] != "internal":
                raise ValueError("Unexpected public load balancer")
            load_balancers.append(lb)
            if not groups:
                act(elb, "delete_load_balancer", LoadBalancerArn=lb["LoadBalancerArn"])
    report["remaining"]["load_balancers"] = len(load_balancers)
    targets = []
    for target in all_pages(elb, "describe_target_groups", "TargetGroups"):
        tags = elb.describe_tags(ResourceArns=[target["TargetGroupArn"]])["TagDescriptions"][0]["Tags"]
        if owned(tags, config):
            targets.append(target)
            if not groups and not target.get("LoadBalancerArns"):
                act(elb, "delete_target_group", TargetGroupArn=target["TargetGroupArn"])
    report["remaining"]["target_groups"] = len(targets)

    volumes = all_pages(ec2, "describe_volumes", "Volumes", Filters=filters)
    report["remaining"]["volumes"] = len(volumes)
    for volume in volumes:
        if not owned(volume.get("Tags"), config):
            raise ValueError("Volume ownership mismatch")
        if volume["State"] == "available" and not volume.get("Attachments"):
            act(ec2, "delete_volume", VolumeId=volume["VolumeId"])

    if cluster and not groups:
        addons = all_pages(eks, "list_addons", "addons", clusterName=config["cluster"])
        for name in addons:
            addon = eks.describe_addon(clusterName=config["cluster"], addonName=name)["addon"]
            if addon["status"] != "DELETING":
                act(eks, "delete_addon", clusterName=config["cluster"], addonName=name, preserve=False)
        if not addons and cluster["status"] != "DELETING":
            act(eks, "delete_cluster", name=config["cluster"])
    report["remaining"]["cluster"] = int(cluster is not None)

    # No worker or operator needs egress once teardown begins. Deleting these
    # independently also handles a partially failed Terraform apply.
    nats = [n for n in all_pages(ec2, "describe_nat_gateways", "NatGateways", Filter=filters) if n["State"] != "deleted"]
    for nat in nats:
        if not owned(nat.get("Tags"), config):
            raise ValueError("NAT ownership mismatch")
        if nat["State"] != "deleting":
            act(ec2, "delete_nat_gateway", NatGatewayId=nat["NatGatewayId"])
    report["remaining"]["nat_gateways"] = len(nats)
    endpoints = all_pages(ec2, "describe_vpc_endpoints", "VpcEndpoints", Filters=filters)
    for endpoint in endpoints:
        if not owned(endpoint.get("Tags"), config):
            raise ValueError("Endpoint ownership mismatch")
        if endpoint["State"] not in ("deleting", "deleted"):
            act(ec2, "delete_vpc_endpoints", VpcEndpointIds=[endpoint["VpcEndpointId"]])
    report["remaining"]["endpoints"] = len(endpoints)
    addresses = ec2.describe_addresses(Filters=filters).get("Addresses", [])
    for address in addresses:
        if not owned(address.get("Tags"), config):
            raise ValueError("Address ownership mismatch")
        if not address.get("AssociationId") and not address.get("NetworkInterfaceId"):
            act(ec2, "release_address", AllocationId=address["AllocationId"])
    report["remaining"]["addresses"] = len(addresses)
    # Route53 has a monthly zone charge. Delete only this run's exact private
    # namespace; record sets are synthetic and belong to this newly created zone.
    dns = clients["route53"]
    zones = []
    for zone in all_pages(dns, "list_hosted_zones", "HostedZones"):
        if zone["Name"] != config["cluster"] + ".internal." or not zone["Config"]["PrivateZone"]:
            continue
        zone_id = zone["Id"].split("/")[-1]
        tags = dns.list_tags_for_resource(ResourceType="hostedzone", ResourceId=zone_id)["ResourceTagSet"]["Tags"]
        if not owned(tags, config):
            continue
        zones.append(zone_id)
        records = [r for r in all_pages(dns, "list_resource_record_sets", "ResourceRecordSets", HostedZoneId=zone_id) if r["Type"] not in ("SOA", "NS")]
        if records:
            act(dns, "change_resource_record_sets", HostedZoneId=zone_id, ChangeBatch={"Changes": [{"Action": "DELETE", "ResourceRecordSet": r} for r in records]})
        else:
            act(dns, "delete_hosted_zone", Id=zone_id)
    report["remaining"]["private_zones"] = len(zones)
    report["cost_resources_absent"] = not any(report["remaining"].values())
    return report


def handler(event, context):
    import boto3
    from botocore.config import Config

    config = json.loads(os.environ["LAB_CONFIG"])
    sdk = Config(connect_timeout=3, read_timeout=15, retries={"mode": "standard", "max_attempts": 2})
    clients = {name: boto3.client(name, region_name=config["region"], config=sdk)
               for name in ("sts", "ec2", "eks", "elbv2", "route53", "s3", "events", "cloudwatch")}
    now = datetime.now(timezone.utc)
    due = validate(config, clients["sts"].get_caller_identity()["Account"], now)
    inspect = event == {"mode": "inspect"}
    scheduled = (event.get("source") == "aws.events" and event.get("account") == config["account"]
                 and event.get("resources") == [config["rule_arn"]])
    if not inspect and not scheduled:
        raise ValueError("Only the exact schedule or read-only inspection is accepted")
    if not due and not inspect:
        return {"status": "armed", "cleanup_after": config["cleanup_after"]}
    report = reconcile(config, clients, execute=due and not inspect)
    report.update(checked_at=now.isoformat(), deadline=config["deadline"])
    clients["s3"].put_object(Bucket=config["bucket"], Key=config["result_key"],
        Body=json.dumps(report).encode(), ContentType="application/json", ServerSideEncryption="AES256",
        ExpectedBucketOwner=config["account"])
    print(json.dumps(report))
    if due and not inspect:
        overdue = now >= datetime.fromisoformat(config["deadline"]) and not report["cost_resources_absent"]
        clients["cloudwatch"].put_metric_data(Namespace="DurableJobsLab", MetricData=[{
            "MetricName": "CleanupOverdue", "Dimensions": [{"Name": "RunId", "Value": config["run_id"]}],
            "Value": int(overdue), "Unit": "Count"}])
    if due and not inspect and report["cost_resources_absent"]:
        clients["events"].disable_rule(Name=config["rule_name"])
    return report
