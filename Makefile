GO        ?= go
BINARY    := aura
PKG       := github.com/anggasct/aura/internal/cli
CAPS_PKG  := github.com/anggasct/aura/internal/capability
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell if [ -n "$$SOURCE_DATE_EPOCH" ]; then date -u -d "@$$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ; else date -u +%Y-%m-%dT%H:%M:%SZ; fi)
PROFILE   ?= core
CAPABILITIES ?=
LDFLAGS   := -s -w -X $(PKG).version=$(VERSION) -X $(PKG).commit=$(COMMIT) -X $(PKG).date=$(DATE) -X $(CAPS_PKG).buildProfile=$(PROFILE) -X $(CAPS_PKG).buildCapabilities=$(CAPABILITIES)

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build build-all test vet fmt-check lint verify clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/aura

build-all:
	mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch ./cmd/aura || exit 1; \
	done

test:
	$(GO) test -v -race ./...

vet:
	$(GO) vet ./...

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files found:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

lint:
	$(GO) tool golangci-lint run ./...

verify: build-all fmt-check vet test lint

clean:
	rm -f $(BINARY)
	rm -rf dist
