BINARY  := zeus-gslb
PKG     := ./cmd/zeus-gslb
DIST    := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

GO := go

# Fully static binaries (Go pure-Go net resolver + miekg/dns, no cgo) so the
# image can run on distroless/static with zero shared-library dependencies.
export CGO_ENABLED := 0

.PHONY: all build release checksums push clean tidy vet test docker

# Build for the host platform into dist/.
build:
	mkdir -p $(DIST)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY) $(PKG)

# Cross-compile the binaries the overlay-dns deployment pulls (linux amd64 +
# arm64 — mirrors zeus-agent-go's platform matrix for the same reason: the
# fleet mixes both architectures).
release: clean
	mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 $(PKG)
	GOOS=linux GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64 $(PKG)
	$(MAKE) checksums

# Generate dist/checksums.txt (sha256 of both release binaries).
checksums:
	cd $(DIST) && sha256sum $(BINARY)-linux-amd64 $(BINARY)-linux-arm64 > checksums.txt

# zeus-gslb ships as a container image (rolled via the overlay-dns deployment
# reconcile, per plan §8), not a bare downloaded binary like zeus-agent — so
# there is no bucket-push target to mirror yet. `make docker` + a registry
# push is the release path; wire it up here once that registry is decided.
push:
	@echo "no bucket-push target for zeus-gslb — release path is 'make docker' + registry push (TODO)"
	@exit 1

# Build the multi-stage container image for the host platform.
docker:
	docker build -t zeus-gslb:$(VERSION) .

tidy:
	$(GO) mod tidy

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

clean:
	rm -rf $(DIST)
