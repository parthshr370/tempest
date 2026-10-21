# harness — pure-Go in-sandbox agent
# Follow the implementation plan for the fragment build order (F0–F20) and
# TDD gates. Never advance a fragment on a red test.

GO ?= go
PKGS ?= ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILD_LDFLAGS := -X go.harness.dev/harness/internal/build.Version=$(VERSION) -X go.harness.dev/harness/internal/build.Commit=$(COMMIT) -X go.harness.dev/harness/internal/build.Date=$(DATE)

.PHONY: all build build-examples test test-race test-integration test-contract smoke tidy vet clean check

all: build test

check: build build-examples vet test test-race

build:
	$(GO) build -ldflags "$(BUILD_LDFLAGS)" $(PKGS)

build-examples:
	$(GO) -C examples/embed build -o /dev/null ./...

# Unit — fast, hermetic, no network/Docker. The default gate for every fragment.
test:
	$(GO) test -count=1 $(PKGS)

# Unit under the race detector — MUST be green for any fragment touching goroutines
# (streaming, parallel tools, queues). See 02-map-ai-agent.md concurrency notes.
test-race:
	$(GO) test -race -count=1 $(PKGS)

# Integration — real provider (Anthropic) / real subprocess (rg, fd, bash).
# Gated behind the `integration` build tag + env (e.g. ANTHROPIC_API_KEY) so `test` stays hermetic.
test-integration:
	$(GO) test -tags=integration -count=1 $(PKGS)

# Contract — the FE streaming contract (Guardrail #1): assert the adapter's
# stream-json / SSE frames match what the backend+frontend expect. Gated by tag.
test-contract:
	$(GO) test -tags=contract -count=1 $(PKGS)

# Smoke — end-to-end in a scratch dir ("create file X, then edit it" in Phase 1).
smoke: build
	@bash scripts/smoke.sh

vet:
	$(GO) vet $(PKGS)

tidy:
	$(GO) mod tidy

clean:
	$(GO) clean
	rm -f coverage.out
