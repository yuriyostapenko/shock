# SHOCK developer targets. Pin versions here; CI uses the same targets.
GO            ?= go
GOLANGCI_LINT ?= $(shell $(GO) env GOPATH)/bin/golangci-lint
SETUP_ENVTEST ?= $(shell $(GO) env GOPATH)/bin/setup-envtest
ENVTEST_K8S   ?= 1.35.x
KIND          ?= kind
KIND_CLUSTER  ?= shock-e2e
KIND_NODE_IMAGE ?= kindest/node:v1.35.8
AGENT_SANDBOX_VERSION ?= v1.0.2
IMAGE         ?= shock:dev
CHANNEL       ?= latest
RUNNER_IMAGE  ?= shock-runner:dev
CHART         := charts/shock
E2E_NAMESPACE ?= shock-e2e
export AGENT_SANDBOX_VERSION E2E_NAMESPACE

.PHONY: all build fmt vet lint test envtest helm-lint helm-template chart-golden trusted-domains bump-claude e2e-kind e2e-setup e2e e2e-teardown image image-runner clean

all: fmt vet lint test helm-lint chart-golden

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/shock ./cmd/shock

fmt:
	gofmt -l -w cmd internal test

vet:
	$(GO) vet ./...

lint:
	$(GOLANGCI_LINT) run ./...

test:
	$(GO) test ./internal/... ./cmd/...

# envtest: real kube-apiserver + etcd for the API concurrency tests.
envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) -p path)" $(GO) test -tags envtest ./test/envtest/... -count=1

helm-lint:
	helm lint $(CHART) -f test/values/minimal.yaml

helm-template:
	helm template shock $(CHART) -n shock -f test/values/minimal.yaml --kube-version 1.35.0

# The rendered sandbox-template must strict-decode into the typed Sandbox.
# Regenerates charts/shock/files/anthropic-trusted-domains.txt.
trusted-domains:
	./hack/update-trusted-domains.sh

# Pins images/*/Dockerfile and the chart annotation to Anthropic's current
# Claude Code release (CHANNEL=latest|stable); CHANNEL=check only verifies them.
bump-claude:
	./hack/bump-claude.sh $(CHANNEL)

chart-golden:
	$(GO) test ./test/chart/... -count=1

image:
	docker build -f images/orchestrator/Dockerfile -t $(IMAGE) .

image-runner:
	docker build -f images/runner/Dockerfile -t $(RUNNER_IMAGE) .

e2e-setup:
	mkdir -p bin
	$(KIND) get clusters | grep -qx $(KIND_CLUSTER) || $(KIND) create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) --wait 120s
	curl --retry 8 --retry-delay 3 --retry-all-errors -fsSL -o bin/agent-sandbox-$(AGENT_SANDBOX_VERSION).yaml \
	  https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/sandbox.yaml
	kubectl apply -f bin/agent-sandbox-$(AGENT_SANDBOX_VERSION).yaml
	kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=180s
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o bin/shock-linux ./cmd/shock
	docker build -f test/e2e/Dockerfile.controller -t shock-e2e/controller:dev .
	$(KIND) load docker-image shock-e2e/controller:dev --name $(KIND_CLUSTER)

# Runs the kind e2e suite against the current kube context. Requires e2e-setup.
e2e:
	$(GO) build -o bin/shock ./cmd/shock
	SHOCK_BIN=$(CURDIR)/bin/shock $(GO) test -tags e2e ./test/e2e/... -count=1 -timeout 40m -v

e2e-kind: e2e-setup e2e

e2e-teardown:
	$(KIND) delete cluster --name $(KIND_CLUSTER)

clean:
	rm -rf bin
