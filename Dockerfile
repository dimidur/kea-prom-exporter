# syntax=docker/dockerfile:1.7
#
# Multi-stage, multi-arch build. The runtime image is distroless `static`:
# still no shell, no package manager and no libc, but unlike `scratch` it
# carries the two things this exporter actually needs from a filesystem —
# a CA bundle (Kea's control socket can be configured `socket-type: https`,
# which on scratch fails with "certificate signed by unknown authority") and
# a real /etc/passwd entry for the non-root user it runs as.
#
# Build for the local arch:
#   docker build -t kea-prom-exporter:dev .
#
# Build multi-arch (requires `docker buildx` + a builder):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t dimidur/kea-prom-exporter:<tag> --push .

FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS build
ARG TARGETOS
ARG TARGETARCH
# Fail loudly if the builder tag and go.mod's toolchain line drift apart,
# rather than silently downloading a toolchain mid-build -- which is a slow,
# obscure failure behind an egress proxy.
ENV GOTOOLCHAIN=local
WORKDIR /src

# Cache module downloads independently of source edits.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/kea-prom-exporter .

# The :nonroot tag runs as uid 65532, a dedicated identity with its own
# /etc/passwd entry — not 65534/nobody, which is the shared NFS-anonymous uid
# that a dozen unrelated things already map to. Kubernetes runAsNonRoot and
# PodSecurity `restricted` reject an image whose user is root.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/kea-prom-exporter /kea-prom-exporter
# Default Prometheus port for Kea exporters (matches the mweinelt
# heritage). Override with --listen or LISTEN env var.
EXPOSE 9547
ENTRYPOINT ["/kea-prom-exporter"]
