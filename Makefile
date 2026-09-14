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
CHART         := charts/shock
E2E_NAMESPACE ?= shock-e2e
export AGENT_SANDBOX_VERSION E2E_NAMESPACE

.PHONY: all build fmt vet lint test envtest helm-lint helm-template chart-golden e2e-kind e2e-setup e2e e2e-teardown image clean

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

# envtest runs a real kube-apiserver + etcd: resourceVersion conflicts, UID
# delete preconditions, immutable Secrets, generation bumps.
envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) -p path)" $(GO) test -tags envtest ./test/envtest/... -count=1

helm-lint:
	helm lint $(CHART) -f test/values/minimal.yaml

helm-template:
	helm template shock $(CHART) -n shock -f test/values/minimal.yaml --kube-version 1.35.0

# The rendered sandbox-template must strict-decode into the typed Sandbox.
chart-golden:
	$(GO) test ./test/chart/... -count=1

image:
	docker build -f images/orchestrator/Dockerfile -t $(IMAGE) .

e2e-setup:
	$(KIND) get clusters | grep -qx $(KIND_CLUSTER) || $(KIND) create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) --wait 120s
	kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/sandbox.yaml
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
