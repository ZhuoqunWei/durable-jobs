# Pinned controller IAM reference

`alb-iam-v3.5.0.json` is the upstream Apache-2.0 reference from
https://raw.githubusercontent.com/kubernetes-sigs/aws-load-balancer-controller/v3.5.0/docs/install/iam_policy.json
retrieved September 28, 2026. SHA-256:
`16f232c9d9f79366fe949c4550ad517a202380058a9e48d45a4e215044a20a6a`.
The upstream LICENSE is included here.

This document is NOT attached verbatim to a role. `alb.tf` derives a smaller
policy: no IAM or EC2 mutation, no WAF/Shield/Cognito, no Route53, no NLB resource
ARNs; ELB writes are limited to the selected region/account and existing cluster
tag conditions require an exact cluster match. Internet-facing creation is denied.
Some listener operations retain account-wide scope within the selected region;
this is not a fully least-privileged production policy. AWS IAM evaluation and
controller reconciliation require a real approved run before operational claims.
