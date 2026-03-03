package main

import (
	"fmt"
	"os"

	"github.com/anthropics/gt-operator/internal/shim"
)

// tmux-shim binary — drop-in replacement for tmux in the operator container.
// Routes tmux commands to local tmux or kubectl exec based on session→pod mapping.
//
// Usage: Place this binary at /usr/local/bin/tmux in the operator container.
// Set GT_REAL_TMUX=/usr/bin/tmux to point to the real tmux binary.
// Set GT_TMUX_SHIM=1 to indicate we're running inside the shim (prevents recursion).
//
// The session→pod mapping is loaded from a shared file maintained by the operator.

const registryPath = "/var/run/gt-operator/session-registry.json"

func main() {
	os.Setenv("GT_TMUX_SHIM", "1")

	// TODO: Load session registry from shared file or connect to operator API
	router := shim.NewMapRouter()

	namespace := os.Getenv("GT_NAMESPACE")
	if namespace == "" {
		namespace = "gastown"
	}

	s := shim.NewShim(router, namespace)
	if err := s.Exec(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "tmux-shim: %v\n", err)
		os.Exit(1)
	}
}
