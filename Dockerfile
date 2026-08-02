# syntax=docker/dockerfile:1.7
#
# Multi-stage, multi-arch build. The runtime image is `scratch` —
# kea-prom-exporter is a static Go binary with no runtime deps.
#
# Build for the local arch:
#   docker build -t kea-prom-exporter:dev .
#
# Build multi-arch (requires `docker buildx` + a builder):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t dimidur/kea-prom-exporter:<tag> --push .

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

# Cache module downloads independently of source edits.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/kea-prom-exporter .

FROM scratch
COPY --from=build /out/kea-prom-exporter /kea-prom-exporter
# Default Prometheus port for Kea exporters (matches the mweinelt
# heritage). Override with --listen or LISTEN env var.
EXPOSE 9547
ENTRYPOINT ["/kea-prom-exporter"]
