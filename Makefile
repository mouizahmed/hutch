VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/mouizahmed/hutch/internal/version.Version=$(VERSION)

.PHONY: build linux test vet vuln check clean images kernel rootfs compat

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

# hutch-guest must be static to run in any guest image.
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/linux-amd64/ ./cmd/hutchd ./cmd/hutch-guest
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/linux-arm64/ ./cmd/hutchd ./cmd/hutch-guest

test:
	go test ./...

vet:
	go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

check: vet test vuln

# Builds every guest in compat/guests.txt and tests the current client against it.
compat:
	docker run --rm --privileged -v $(CURDIR):/src -w /src golang@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 compat/run.sh

images: kernel rootfs

kernel:
	images/kernel/build.sh dist/images

rootfs: linux
	images/rootfs/build.sh dist/images bin/linux-amd64/hutch-guest

clean:
	rm -rf bin dist
