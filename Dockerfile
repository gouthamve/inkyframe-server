# syntax=docker/dockerfile:1

# Build stage runs natively on the build platform and cross-compiles to the
# target arch. The deps are pure Go and CGO is disabled, so multi-arch builds
# need no QEMU emulation — the only target-arch step is a static COPY.
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/inkyframe-server .

# Minimal runtime: static distroless image. It ships CA certificates (needed for
# the Immich HTTPS calls) and a non-root user, and contains nothing else. The
# server writes nothing to disk; mount the config in at the path below.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/inkyframe-server /usr/local/bin/inkyframe-server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/inkyframe-server"]
CMD ["-config", "/etc/inkyframe/config.yaml"]
