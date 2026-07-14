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

# DO NOT give these ARGs default values. buildx pre-declares TARGETOS/TARGETARCH
# and populates them per target platform — but ONLY if we redeclare them bare.
# Writing `ARG TARGETARCH=amd64` makes the default win, so `--platform
# linux/arm64` still compiles GOARCH=amd64 and buildx then publishes that amd64
# binary under the arm64 entry of the manifest list. The manifest is correct and
# the content is a lie; arm64 nodes (GKE T2A, Graviton) die with `exec format
# error`. That is exactly what shipped in 0.1.0.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN test -n "$TARGETARCH" || { echo "FATAL: TARGETARCH empty — build with buildx"; exit 1; }
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/zeus-gslb ./cmd/zeus-gslb

# Assert the binary we just produced really is for the platform this image will
# CLAIM to be. Checked against TARGETPLATFORM (not TARGETARCH) on purpose: the
# 0.1.0 bug was TARGETARCH silently holding the wrong value, so an assertion
# phrased in terms of TARGETARCH would have compared the mistake against itself
# and passed. TARGETPLATFORM is the platform buildx will actually file this
# image under in the manifest list, so it is the only trustworthy source here.
#
# `go version -m` reports the settings baked into the binary — what we BUILT —
# with no QEMU and no need to execute a foreign-arch binary.
ARG TARGETPLATFORM
RUN set -eu; \
    want="${TARGETPLATFORM#*/}"; \
    got="$(go version -m /out/zeus-gslb | sed -n 's/.*[[:space:]]GOARCH=\([a-z0-9]*\).*/\1/p' | head -1)"; \
    [ -n "$got" ] || { echo "FATAL: could not read GOARCH from binary"; exit 1; }; \
    [ "$got" = "$want" ] || { \
      echo "FATAL: publishing as '$TARGETPLATFORM' but binary is GOARCH=$got"; exit 1; }; \
    echo "arch OK: $TARGETPLATFORM carries GOARCH=$got"

# distroless/static: no libc, no shell — the binary above is 100% static so
# this is sufficient (and smaller/safer than distroless/base or alpine).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/zeus-gslb /usr/local/bin/zeus-gslb

# UDP+TCP DNS.
EXPOSE 5355/udp 5355/tcp

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/zeus-gslb"]
CMD ["--mode=responder", "--listen=:5355", "--bundle=/etc/zeus-gslb/bundle.json", "--cache-dir=/var/lib/zeus-gslb"]
