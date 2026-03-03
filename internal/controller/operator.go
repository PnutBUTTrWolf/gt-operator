package controller

import (
	"context"
	"fmt"
	"log"
)

// Config holds operator configuration.
type Config struct {
	Kubeconfig string
	Namespace  string
	AgentImage string
}

// Operator manages the Gas Town control plane in Kubernetes.
// It watches Polecat and Rig CRDs, reconciles desired state into pods,
// and runs the gt daemon binary with the tmux shim for cross-pod operations.
type Operator struct {
	config Config
}

// New creates a new Operator instance.
func New(cfg Config) (*Operator, error) {
	return &Operator{config: cfg}, nil
}

// Run starts the operator's reconciliation loops.
func (o *Operator) Run(ctx context.Context) error {
	log.Printf("[operator] starting in namespace %s (image: %s)", o.config.Namespace, o.config.AgentImage)

	// TODO: Initialize k8s client from kubeconfig or in-cluster config
	// TODO: Register CRD informers for Polecat and Rig resources
	// TODO: Start reconciliation loops:
	//   - Polecat controller: CRD created → create Pod; CRD deleted → delete Pod
	//   - Rig controller: CRD created → create witness + refinery Deployments
	//   - Daemon runner: start gt daemon binary with GT_RUNTIME=k8s
	// TODO: Start leader election (only one operator instance active)

	<-ctx.Done()
	log.Println("[operator] shutting down")
	return fmt.Errorf("context cancelled: %w", ctx.Err())
}
