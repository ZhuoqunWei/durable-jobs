FROM hashicorp/terraform:1.16.4@sha256:985cdc6c1d9b0a65b83377f666efd2f740b47f02ac55be1ced3d18f7d3b0e829

WORKDIR /src
COPY .terraform.lock.hcl versions.tf ./
# Build has network access solely to install pinned public provider packages.
# No credentials, actual tfvars, Terraform state or host home are copied.
RUN terraform init -backend=false -input=false -lockfile=readonly
COPY *.tf ./
COPY tests/ ./tests/
COPY reference/alb-iam-v3.5.0.json ./reference/alb-iam-v3.5.0.json
COPY check.sh ./check.sh
ENV TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1
USER 65532:65532
WORKDIR /work
ENTRYPOINT ["/bin/sh", "/src/check.sh"]
