# Set the shell to bash always
SHELL := /bin/bash

# Every file `make generate` writes.
GENERATED := ':(glob)apis/**/zz_generated.*.go' helm/incident-provider-crds/templates

IMAGE ?= ghcr.io/krateo-platformops/incident-provider
CHECK_IMAGE ?= ghcr.io/krateo-platformops/incident-provider-check
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The control plane of the real-API-server tests (build tag integration). setup-envtest is pinned
# to the head of controller-runtime's release-0.23 branch, the controller-runtime in go.mod.
ENVTEST_K8S_VERSION ?= 1.35.x
SETUP_ENVTEST ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.0.0-20260305142021-f9589b9f2b9d

.PHONY: help
help: ## print this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: generate
generate: ## generate deepcopy methodsets and the Incident CRD (into helm/incident-provider-crds)
	cd apis && go generate -tags generate ./...

.PHONY: check-generate
check-generate: generate ## fail if the committed generated files differ from a fresh generate
	@git diff --exit-code -- $(GENERATED) || { echo "generated files are stale: run 'make generate' and commit"; exit 1; }
	@untracked="$$(git ls-files --others --exclude-standard -- $(GENERATED))"; \
	  if [ -n "$$untracked" ]; then echo "untracked generated files: $$untracked"; exit 1; fi

.PHONY: build
build: ## build all packages
	go build ./...

.PHONY: test
test: ## run unit tests
	go test -race -cover ./...

.PHONY: test-integration
test-integration: ## run the real-API-server (envtest) tests
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test -race -count=1 -tags integration -run Envtest ./...

.PHONY: lint
lint: ## run go vet, the integration tests included
	go vet -tags integration ./...

.PHONY: lint-charts
lint-charts: ## helm lint and render every chart, with CHART_VERSION set to 0.0.0
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	  for c in helm/*/; do \
	    n="$$(basename "$$c")"; cp -R "$$c" "$$tmp/$$n"; \
	    sed -i.bak 's/CHART_VERSION/0.0.0/g' "$$tmp/$$n/Chart.yaml"; \
	    helm lint --strict "$$tmp/$$n" && helm template "$$n" "$$tmp/$$n" >/dev/null || exit 1; \
	  done

.PHONY: image
image: ## build the controller image
	docker build -t $(IMAGE):$(VERSION) .

.PHONY: image-check
image-check: ## build the check pod image
	docker build -t $(CHECK_IMAGE):$(VERSION) check

.PHONY: check
check: build lint test test-integration check-generate lint-charts ## everything CI runs
