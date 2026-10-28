"""Scheduled cleanup proof for ONE newly allocated, unused Elastic IP.

Not a cluster teardown function. Exact ID + account + tags + expiry are required.
The Lambda role must additionally limit ReleaseAddress to that exact tagged ARN.
No credentials or sensitive application data enter the event, environment, or log.
"""

from datetime import datetime, timezone
import json
import os
import re


def cleanup(config, caller_account, ec2, now):
    required = {"account", "region", "allocation_id", "run_id", "not_before"}
    if set(config) != required or config["account"] != caller_account:
        raise ValueError("Invalid config or wrong account")
    if not re.fullmatch(r"[0-9]{12}", config["account"]) or config["region"] != "us-east-1":
        raise ValueError("Invalid target")
    if not re.fullmatch(r"eipalloc-[a-f0-9]{17}", config["allocation_id"]):
        raise ValueError("One explicit allocation ID is required")
    if not re.fullmatch(r"canary-[0-9]{8}T[0-9]{6}Z", config["run_id"]):
        raise ValueError("Invalid run ID")
    expiry = datetime.fromisoformat(config["not_before"].replace("Z", "+00:00"))
    if expiry.tzinfo is None or now.tzinfo is None:
        raise ValueError("Timezone required")
    if now < expiry:
        raise ValueError("Cleanup cannot run before its approved expiry")
    try:
        addresses = ec2.describe_addresses(AllocationIds=[config["allocation_id"]])["Addresses"]
    except Exception as error:
        # Idempotent only for this specific missing-resource response; an access
        # failure is never treated as successful cleanup.
        if getattr(error, "response", {}).get("Error", {}).get("Code") == "InvalidAllocationID.NotFound":
            return {"status": "already_absent", "allocation_id": config["allocation_id"]}
        raise
    if len(addresses) != 1 or addresses[0].get("AllocationId") != config["allocation_id"]:
        raise ValueError("Unexpected address inventory")
    address = addresses[0]
    tags = {item["Key"]: item["Value"] for item in address.get("Tags", [])}
    expected = {"Project": "durable-jobs-lab", "Purpose": "cleanup-canary", "LabRun": config["run_id"]}
    if any(tags.get(key) != value for key, value in expected.items()):
        raise ValueError("Ownership tags mismatch; refusing cleanup")
    if address.get("AssociationId") or address.get("NetworkInterfaceId") or address.get("InstanceId"):
        raise ValueError("Address is in use; refusing cleanup")
    if address.get("Domain") != "vpc":
        raise ValueError("Unexpected address domain")
    ec2.release_address(AllocationId=config["allocation_id"])
    return {"status": "release_requested", "allocation_id": config["allocation_id"]}


def handler(event, context):
    import boto3  # Provided by the Lambda Python runtime; not required by offline tests.
    from botocore.config import Config

    config = json.loads(os.environ["CANARY_CONFIG"])
    if event != {"run_id": config["run_id"]}:
        raise ValueError("Unexpected invocation")
    sdk = Config(connect_timeout=3, read_timeout=10, retries={"max_attempts": 2, "mode": "standard"})
    account = boto3.client("sts", region_name=config["region"], config=sdk).get_caller_identity()["Account"]
    result = cleanup(config, account, boto3.client("ec2", region_name=config["region"], config=sdk),
                     datetime.now(timezone.utc))
    print(json.dumps({"run_id": config["run_id"], "request_id": context.aws_request_id, **result}))
    return result
