# syntax=docker/dockerfile:1
# Pin the multi-platform index, not one developer's CPU architecture.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS source
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY Makefile ./

# This stage has the compiler/C toolchain needed by the race detector.
# It is only used for checks, never shipped as the runtime image.
FROM source AS checks
RUN go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
ENV STATICCHECK=/go/bin/staticcheck
CMD ["make", "lint", "check", "integration"]

FROM source AS build
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/ ./cmd/api ./cmd/worker ./cmd/migrate ./cmd/smoke

# No shell, package manager, compiler, source files, or Docker socket.
FROM scratch AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ /
USER 65532:65532
ENV HTTP_ADDR=0.0.0.0:8085
EXPOSE 8085
ENTRYPOINT ["/api"]
