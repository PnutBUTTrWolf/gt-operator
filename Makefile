.PHONY: build build-shim image install deploy clean test help

BINARY := gt-operator
SHIM_BINARY := tmux-shim
BUILD_DIR := bin
IMAGE_NAME := gt-operator
IMAGE_TAG := latest

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

# --- Test ---

test:
	go test ./...

# --- Clean ---

clean:
	rm -rf $(BUILD_DIR)

# --- Help ---

help:
	@echo "Targets:"
	@echo "  build        Build the operator binary"
	@echo "  build-shim   Build the tmux shim binary"
	@echo "  build-all    Build everything"
	@echo "  image        Build operator container image"
	@echo "  image-agent  Build universal agent container image"
	@echo "  install-crds Apply CRDs to cluster"
	@echo "  deploy       Deploy operator via Helm"
	@echo "  undeploy     Remove operator and CRDs"
	@echo "  test         Run tests"
	@echo "  clean        Remove build artifacts"
