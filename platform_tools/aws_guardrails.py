"""Pure builders for the approved short AWS lab; importing performs no AWS calls.

Bootstrap resources deliberately outlive the disposable lab. This is not an
infrastructure provisioner, hard cost cap, or complete six-hour teardown system.
"""

from datetime import datetime, timedelta, timezone
import re

PROJECT = "durable-jobs-lab"
STATE_KEYS = ("state/durable-jobs-lab.tfstate", "checks/bootstrap.tfstate")
PUBLIC_BLOCK = {key: True for key in (
    "BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets")}


def validate(account, region, bucket):
    if not re.fullmatch(r"[0-9]{12}", account) or region != "us-east-1":
        raise ValueError("Select a 12-digit account and the approved us-east-1 region")
    if bucket != f"durable-jobs-state-{account}-{region}":
        raise ValueError("Use the dedicated account/region state bucket")


def budget_request(account, email, now):
    validate(account, "us-east-1", f"durable-jobs-state-{account}-us-east-1")
    if not re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", email) or now.tzinfo is None:
        raise ValueError("An explicit email and timezone-aware date are required")
    start = now.astimezone(timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0)
    end = start + timedelta(days=2)
    return {
        "AccountId": account,
        "Budget": {
            "BudgetName": "durable-jobs-first-lab-" + start.strftime("%Y%m%d"),
            "BudgetLimit": {"Amount": "50", "Unit": "USD"},
            "BudgetType": "COST", "TimeUnit": "CUSTOM",
            "TimePeriod": {"Start": start.isoformat(), "End": end.isoformat()},
            # Include unrelated costs in the short window conservatively; do
            # not let credits/refunds hide gross spending. No delayed tag filter.
            "CostTypes": {"IncludeCredit": False, "IncludeRefund": False,
                          "UseBlended": False, "UseAmortized": False},
        },
        "NotificationsWithSubscribers": [
            {"Notification": {"NotificationType": "ACTUAL", "ComparisonOperator": "GREATER_THAN",
                              "Threshold": dollars, "ThresholdType": "ABSOLUTE_VALUE"},
             "Subscribers": [{"SubscriptionType": "EMAIL", "Address": email}]}
            for dollars in (5, 10, 25, 40, 50)
        ],
    }


def policy(statements):
    return {"Version": "2012-10-17", "Statement": statements}


def operator_trust(account, user_arn):
    if not re.fullmatch(rf"arn:aws:iam::{account}:user/[A-Za-z0-9+=,.@_/-]+", user_arn):
        raise ValueError("Only the explicitly approved same-account IAM user may assume this role")
    return policy([{"Effect": "Allow", "Principal": {"AWS": user_arn}, "Action": "sts:AssumeRole"}])


def operator_policy(account, region, bucket):
    validate(account, region, bucket)
    bucket_arn = f"arn:aws:s3:::{bucket}"
    keys = [bucket_arn + "/" + key for key in STATE_KEYS]
    locks = [key + ".tflock" for key in keys]
    return policy([
        {"Sid": "StateBucketMetadata", "Effect": "Allow", "Action": [
            "s3:GetBucketLocation", "s3:GetBucketVersioning", "s3:GetEncryptionConfiguration",
            "s3:GetBucketPublicAccessBlock"], "Resource": bucket_arn},
        {"Sid": "ListStatePrefixes", "Effect": "Allow", "Action": "s3:ListBucket",
         "Resource": bucket_arn, "Condition": {"StringLike": {"s3:prefix": ["state/*", "checks/*"]}}},
        {"Sid": "ReadWriteStateNotDelete", "Effect": "Allow",
         "Action": ["s3:GetObject", "s3:PutObject"], "Resource": keys},
        {"Sid": "LockfilesOnly", "Effect": "Allow",
         "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": locks},
        {"Sid": "PublicRegionalVersionMetadata", "Effect": "Allow", "Resource": "*",
         "Action": ["eks:DescribeClusterVersions", "eks:DescribeAddonVersions", "eks:DescribeAddonConfiguration"],
         "Condition": {"StringEquals": {"aws:RequestedRegion": region}}},
        {"Sid": "LabClusterMetadata", "Effect": "Allow", "Action": "eks:DescribeCluster",
         "Resource": f"arn:aws:eks:{region}:{account}:cluster/{PROJECT}"},
        {"Sid": "LabImageMetadata", "Effect": "Allow", "Action": "ecr:DescribeImages",
         "Resource": f"arn:aws:ecr:{region}:{account}:repository/durable-jobs"},
        {"Sid": "CertificateMetadata", "Effect": "Allow", "Action": "acm:DescribeCertificate",
         "Resource": f"arn:aws:acm:{region}:{account}:certificate/*"},
        {"Sid": "ELBServiceRoleMetadata", "Effect": "Allow", "Action": "iam:GetRole",
         "Resource": f"arn:aws:iam::{account}:role/aws-service-role/elasticloadbalancing.amazonaws.com/AWSServiceRoleForElasticLoadBalancing"},
    ])


def bucket_policy(bucket):
    arn = "arn:aws:s3:::" + bucket
    return policy([{"Sid": "DenyInsecureTransport", "Effect": "Deny", "Principal": "*",
                    "Action": "s3:*", "Resource": [arn, arn + "/*"],
                    "Condition": {"Bool": {"aws:SecureTransport": "false"}}}])


def backend_config(account, bucket):
    validate(account, "us-east-1", bucket)
    return {"bucket": bucket, "key": STATE_KEYS[0], "region": "us-east-1",
            "encrypt": True, "use_lockfile": True, "allowed_account_ids": [account],
            "workspace_key_prefix": "checks/workspaces"}
