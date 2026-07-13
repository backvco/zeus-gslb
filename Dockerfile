# zeus-gslb responder image.
#
# Multi-stage: build a fully static (CGO_ENABLED=0) binary, then run it on
# distroless/static — no shell, no package manager, minimal attack surface,
# matches the "tiny image" requirement in plan §8 (runs as a second container
# in the existing zeus-overlay-dns pod).
#
# Build for the target platform with buildx, e.g.:
#   docker buildx build --platform linux/amd64,linux/arm64 -t zeus-gslb:v0 .
# TARGETOS/TARGETARCH are populated automatically by buildx; a plain
# `docker build` on amd64/arm64 hosts also works via BuildKit's default args.

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/zeus-gslb ./cmd/zeus-gslb

# distroless/static: no libc, no shell — the binary above is 100% static so
# this is sufficient (and smaller/safer than distroless/base or alpine).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/zeus-gslb /usr/local/bin/zeus-gslb

# UDP+TCP DNS.
EXPOSE 5355/udp 5355/tcp

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/zeus-gslb"]
CMD ["--mode=responder", "--listen=:5355", "--bundle=/etc/zeus-gslb/bundle.json", "--cache-dir=/var/lib/zeus-gslb"]
