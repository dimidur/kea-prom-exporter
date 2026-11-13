# syntax=docker/dockerfile:1.7
#
# Multi-stage, multi-arch build. The runtime image is distroless `static`:
# still no shell, no package manager and no libc, but unlike `scratch` it
# carries the two things this exporter actually needs from a filesystem:
# a CA bundle (Kea's control socket can be configured `socket-type: https`,
# which on scratch fails with "certificate signed by unknown authority") and
# a real /etc/passwd entry for the non-root user it runs as.
#
# Build for the local arch:
#   docker build -t kea-prom-exporter:dev .
#
# Build multi-arch (requires `docker buildx` + a builder). Pass the build args
# or the image reports version=dev revision=unknown, which is the problem
# kea_exporter_build_info exists to solve:
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=<tag> --build-arg REVISION=$(git rev-parse HEAD) \
#     -t dimidur/kea-prom-exporter:<tag> --push .

# The base image's own Go only bootstraps the build. The binary is compiled by
# exactly the version in go.mod's `go` line: each RUN below sets GOTOOLCHAIN to
# it, and when that differs from the image's own Go, Go downloads and
# checksum-verifies it. The image itself sets GOTOOLCHAIN=local, which would
# compile with whatever Go it carries.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build
WORKDIR /src

# Cache module downloads, and the toolchain, independently of source edits.
# No ARG is declared above this RUN: every RUN after an ARG misses the cache
# when its value changes, which would repeat the download per target and per
# release.
COPY go.mod go.sum* ./
RUN GOTOOLCHAIN="go$(awk '$1 == "go" {print $2}' go.mod)" go mod download

ARG TARGETOS
ARG TARGETARCH
# Passed in rather than read from the repo: .dockerignore keeps .git out of the
# build context, so Go cannot stamp the VCS revision itself here.
ARG VERSION=dev
ARG REVISION=
COPY . .
# $VERSION and $REVISION are expanded by the shell from the build-arg
# environment, NOT interpolated by BuildKit into the command text: a tag
# containing a quote would otherwise close the -ldflags string and let the
# rest of the tag run as shell.
RUN GOTOOLCHAIN="go$(awk '$1 == "go" {print $2}' go.mod)" \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w -X main.version=$VERSION -X main.revision=$REVISION" \
      -o /out/kea-prom-exporter .

# The :nonroot tag runs as uid 65532, a dedicated identity with its own
# /etc/passwd entry, not 65534/nobody, which is the shared NFS-anonymous uid
# that a dozen unrelated things already map to. Kubernetes runAsNonRoot and
# PodSecurity `restricted` reject an image whose user is root.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/kea-prom-exporter /kea-prom-exporter
# Default Prometheus port for Kea exporters (matches the mweinelt
# heritage). Override with --listen or LISTEN env var.
EXPOSE 9547
ENTRYPOINT ["/kea-prom-exporter"]
