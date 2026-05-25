# syntax=docker/dockerfile:1

# Build stage runs natively on the build platform and cross-compiles to the
# target arch. The Go deps are pure Go and CGO is disabled, so the Go build needs
# no QEMU emulation. (The runtime stage below does apt-get install ffmpeg on the
# target arch, so a cross-arch image build does need emulation for that step.)
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

# Runtime: the movie app shells out to ffmpeg/ffprobe to decode video, which the
# static distroless image can't provide — so we use a Debian slim base and install
# ffmpeg (ffprobe ships with it) plus CA certificates (still needed for the Immich
# HTTPS calls). This is substantially larger than distroless; that's the cost of
# video support. Mount the config and any movie files (read-only is fine); if a
# movie app sets state_file, mount that path writable so it can persist the
# resume position.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --user-group --no-create-home nonroot
COPY --from=build /out/inkyframe-server /usr/local/bin/inkyframe-server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/inkyframe-server"]
CMD ["-config", "/etc/inkyframe/config.yaml"]
