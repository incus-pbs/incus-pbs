VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build test completions release release-check clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/incus-pbs ./cmd/incus-pbs

test:
	go vet ./...
	go test ./...

completions:
	sh scripts/completions.sh

release:
	goreleaser release --snapshot --clean

release-check:
	goreleaser check

clean:
	rm -rf bin build completions
