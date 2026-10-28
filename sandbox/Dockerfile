# Official Python multi-platform digest resolved September 16, 2026.
FROM python:3.13.15-slim-bookworm@sha256:ed86c82274b3c69b52fb5820f358f0bd7df0b603332063cb5c6e32bd220c3e6e
LABEL org.durable-jobs.sandbox.protocol="1"
WORKDIR /opt/durable-jobs
COPY --chmod=0444 runner.py /opt/durable-jobs/runner.py
USER 65534:65534
WORKDIR /tmp
ENTRYPOINT ["/usr/local/bin/python3", "-I", "-B", "/opt/durable-jobs/runner.py"]
