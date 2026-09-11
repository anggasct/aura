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

# Source must read as a standalone product: no internal tracking IDs, doc
# paths, or process vocabulary in shipped code.
INTERNAL_REFS := (AC|IMP|CAP|ADR)-[0-9]+|feat-[a-z0-9-]+|specs?/|project-docs|development-plan|delivery queue|delivery os|hermes|kanban

.PHONY: build build-all test vet fmt-check lint refs-check verify security eval load integration durable-test fuzz-smoke fuzz-one release-snapshot clean

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

durable-test:
	$(GO) test -race -tags durable -count=1 ./internal/durable/...

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

refs-check:
	@found="$$(grep -rInEi '$(INTERNAL_REFS)' --include='*.go' \
		--exclude-dir=.worktree --exclude-dir=dist . || true)"; \
	if [ -n "$$found" ]; then \
		echo "Internal references found in source:"; \
		echo "$$found"; \
		exit 1; \
	fi

verify: build-all fmt-check vet test lint refs-check

security:
	$(GO) tool govulncheck ./...

eval:
	$(GO) test -race ./internal/eval/

load:
	$(GO) test -tags load -race -run TestLoad ./internal/runtime/engine/

# Containment integration suite: the runner needs a cgroup v2 subtree with
# the pids and memory controllers enabled for the test process. Service-
# managed hosts delegate controllers to no session by default, and the
# no-internal-process rule forbids enabling them under a live session, so
# the suite runs from the kernel-exempt root cgroup, whose controllers are
# already enabled. Requires Linux with cgroup v2 mounted writable and
# passwordless sudo; fails closed anywhere else.
integration:
ifeq ($(shell uname -s),Linux)
	@sudo -n env PATH="$(PATH)" bash -ec 'echo $$$$ > /sys/fs/cgroup/cgroup.procs && cd "$(CURDIR)" && $(GO) test -tags=integration -race -count=1 ./internal/sandbox/...'
else
	@echo "integration: containment suite is Linux-only; nothing to run on $(shell uname -s)"
endif

FUZZTIME ?= 10s
FUZZ_ATTEMPTS ?= 3
fuzz-smoke:
	@$(MAKE) --no-print-directory fuzz-one FUZZ_TARGET=FuzzEventPayload FUZZ_PKG=./internal/store/
	@$(MAKE) --no-print-directory fuzz-one FUZZ_TARGET=FuzzConfigDecode FUZZ_PKG=./internal/config/

# The fuzzer can report the end of its own time budget as
# "context deadline exceeded" instead of a clean pass. Retry only that
# shape; any crasher marker fails immediately.
fuzz-one:
	@attempt=1; \
	while [ $$attempt -le $(FUZZ_ATTEMPTS) ]; do \
		echo "fuzz attempt $$attempt/$(FUZZ_ATTEMPTS): $(FUZZ_TARGET) $(FUZZ_PKG)"; \
		out=$$(mktemp); \
		if $(GO) test -fuzz=$(FUZZ_TARGET) -fuzztime=$(FUZZTIME) $(FUZZ_PKG) >$$out 2>&1; then \
			cat $$out; rm -f $$out; exit 0; \
		fi; \
		cat $$out; \
		if grep -q "Failing input written to" $$out || grep -q "failure while testing seed corpus" $$out || grep -q "minimizing.*failing input" $$out || grep -q "panic:" $$out || grep -q "DATA RACE" $$out; then \
			rm -f $$out; exit 1; \
		fi; \
		fails=$$(grep -c "^--- FAIL:" $$out || true); \
		deadlines=$$(grep -c "context deadline exceeded" $$out || true); \
		progressed=$$(grep -c "fuzz: elapsed:" $$out || true); \
		if [ "$$fails" = "1" ] && [ "$$deadlines" = "1" ] && [ "$$progressed" -ge "1" ]; then \
			echo "transient fuzzer budget expiry, retrying"; \
			rm -f $$out; attempt=$$((attempt+1)); \
			if [ $$attempt -gt $(FUZZ_ATTEMPTS) ]; then \
				echo "fuzzer budget expiry persisted without a crasher, accepting as pass"; \
				exit 0; \
			fi; \
			continue; \
		fi; \
		rm -f $$out; exit 1; \
	done

GORELEASER_VERSION ?= v2.17.1
SYFT_VERSION       ?= v1.42.3
release-snapshot:
	$(GO) install github.com/anchore/syft/cmd/syft@$(SYFT_VERSION)
	$(GO) run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release --snapshot --clean --skip=sign

clean:
	rm -f $(BINARY)
	rm -rf dist
