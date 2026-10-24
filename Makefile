# previewd — single entry point for every gate. `make check` is what CI runs.
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

GO          ?= go
GOFLAGS     ?= -p=3
export GOFLAGS
ENVTEST_K8S_VERSION ?= 1.37.0
COVERAGE_MIN ?= 80
COVER_DIR   := coverage

# Resolve envtest binaries: installed copy first (offline), download otherwise.
ENVTEST_ASSETS = $$($(GO) tool setup-envtest use $(ENVTEST_K8S_VERSION) -i -p path 2>/dev/null || $(GO) tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)

.PHONY: all check fmt fmt-check lint vet generate verify-generate test unit cover fuzz demo bench bench-scale build envtest clean help

all: check

help: ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

check: fmt-check verify-generate vet lint test cover ## every gate: format, generated code, vet, lint, tests, coverage

fmt: ## format all Go code
	gofmt -w $$(git ls-files '*.go')

fmt-check: ## fail if any Go file is not gofmt-clean
	@out=$$(gofmt -l $$(git ls-files '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@echo "fmt-check: ok"

vet: ## type-check and vet every package
	$(GO) vet ./...

lint: ## golangci-lint (zero findings)
	golangci-lint run ./...

generate: ## regenerate deepcopy code, the CRD and the controller ClusterRole
	$(GO) tool controller-gen object paths=./api/...
	$(GO) tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd
	$(GO) tool controller-gen rbac:roleName=previewd paths=./internal/controller/... output:rbac:artifacts:config=config/rbac

verify-generate: generate ## fail if generated files differ from what is committed
	@git diff --exit-code --stat -- api config/crd config/rbac || { echo "generated files are stale: commit the output of 'make generate'"; exit 1; }
	@echo "verify-generate: ok"

envtest: ## print the envtest asset directory (downloads once if missing)
	@echo $(ENVTEST_ASSETS)

unit: ## fast tests only (envtest suites skip)
	$(GO) test ./...

test: ## all tests incl. envtest integration (race detector on), with coverage profile
	@mkdir -p $(COVER_DIR)
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" PREVIEWD_REQUIRE_ENVTEST=1 \
	  $(GO) test -race -count=1 -covermode=atomic -coverpkg=./internal/... -coverprofile=$(COVER_DIR)/cover.out ./...

cover: ## enforce the coverage floor on ./internal/... (core logic)
	@total=$$($(GO) tool cover -func=$(COVER_DIR)/cover.out | awk '/^total:/ {sub("%","",$$3); print $$3}'); \
	 echo "coverage (./internal/...): $$total% (minimum $(COVERAGE_MIN)%)"; \
	 awk -v t="$$total" -v m="$(COVERAGE_MIN)" 'BEGIN { exit !(t >= m) }'

fuzz: ## short fuzzing sessions for the parsers
	$(GO) test ./internal/naming -run '^$$' -fuzz FuzzNamespaceIsAlwaysValid -fuzztime 30s
	$(GO) test ./internal/webhook -run '^$$' -fuzz FuzzVerifyNeverPanics -fuzztime 30s

demo: ## offline end-to-end demo against a local API server
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" $(GO) run ./cmd/previewd-demo

bench: ## Go microbenchmarks (render, decide, verify, parse)
	$(GO) test -run '^$$' -bench . -benchmem -benchtime 2s ./internal/render ./internal/lifecycle ./internal/webhook

bench-scale: ## open/close 50 PRs concurrently against envtest and report latencies
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" $(GO) run ./cmd/previewd-demo -scale 50

build: ## build the previewd binary into bin/
	$(GO) build -trimpath -o bin/previewd ./cmd/previewd

clean: ## remove build and coverage output
	rm -rf bin $(COVER_DIR)
