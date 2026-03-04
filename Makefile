.PHONY: build build-shim image install deploy clean test help
.PHONY: deploy-stage1 undeploy-stage1 port-forward-dolt validate-stage1

BINARY := gt-operator
SHIM_BINARY := tmux-shim
BUILD_DIR := bin
IMAGE_NAME := gt-operator
IMAGE_TAG := latest
NAMESPACE := gastown
DOLT_PORT := 3307

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -X main.Version=$(VERSION) \
           -X main.Commit=$(COMMIT) \
           -X main.BuildTime=$(BUILD_TIME)

# --- Build ---

build:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/operator

build-shim:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(SHIM_BINARY) ./internal/shim/cmd

build-all: build build-shim

# --- Image ---

image:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) -f image/Dockerfile .

image-agent:
	docker build -t gt-agent:$(IMAGE_TAG) -f image/Dockerfile.agent .

# --- Deploy ---

install-crds:
	kubectl apply -f deploy/crds/

deploy: install-crds
	helm upgrade --install gt-operator deploy/helm/gt-operator

undeploy:
	helm uninstall gt-operator
	kubectl delete -f deploy/crds/

# --- Stage 1: Dolt only in k8s ---

deploy-stage1:
	@echo "Deploying Stage 1: Dolt StatefulSet only..."
	helm upgrade --install gt-dolt deploy/helm/gt-operator \
		-f deploy/helm/gt-operator/values-stage1.yaml
	@echo ""
	@echo "Waiting for Dolt pod to be ready..."
	kubectl wait --for=condition=ready pod/dolt-0 -n $(NAMESPACE) --timeout=120s
	@echo ""
	@echo "Stage 1 deployed. Next steps:"
	@echo "  make port-forward-dolt   # Forward Dolt to localhost:$(DOLT_PORT)"
	@echo "  make validate-stage1     # Run validation checks"

undeploy-stage1:
	helm uninstall gt-dolt || true
	@echo "Note: PVC dolt-data-dolt-0 is retained. Delete manually if needed:"
	@echo "  kubectl delete pvc dolt-data-dolt-0 -n $(NAMESPACE)"

port-forward-dolt:
	@echo "Port-forwarding Dolt to localhost:$(DOLT_PORT)..."
	@echo "Press Ctrl-C to stop."
	kubectl port-forward -n $(NAMESPACE) svc/dolt-svc $(DOLT_PORT):$(DOLT_PORT)

validate-stage1:
	./scripts/stage1-validate.sh $(NAMESPACE) $(DOLT_PORT)

# --- Test ---

test:
	go test ./...

# --- Clean ---

clean:
	rm -rf $(BUILD_DIR)

# --- Help ---

help:
	@echo "Targets:"
	@echo "  build              Build the operator binary"
	@echo "  build-shim         Build the tmux shim binary"
	@echo "  build-all          Build everything"
	@echo "  image              Build operator container image"
	@echo "  image-agent        Build universal agent container image"
	@echo "  install-crds       Apply CRDs to cluster"
	@echo "  deploy             Deploy full operator stack via Helm"
	@echo "  undeploy           Remove operator and CRDs"
	@echo ""
	@echo "  deploy-stage1      Deploy Stage 1 (Dolt only) to k8s"
	@echo "  undeploy-stage1    Remove Stage 1 deployment (PVC retained)"
	@echo "  port-forward-dolt  Port-forward Dolt to localhost:$(DOLT_PORT)"
	@echo "  validate-stage1    Run Stage 1 validation checks"
	@echo ""
	@echo "  test               Run tests"
	@echo "  clean              Remove build artifacts"
