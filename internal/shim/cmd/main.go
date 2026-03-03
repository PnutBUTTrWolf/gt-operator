package main

import (
	"fmt"
	"os"

	"github.com/anthropics/gt-operator/internal/shim"
)

// tmux-shim binary — drop-in replacement for tmux in all Gas Town containers.
//
// Behavior depends on GT_SHIM_MODE:
//   "operator" — routes remote sessions via kubectl exec (has registry + RBAC)
//   "agent"    — routes remote sessions via nudge queue on shared PVC
//
// If GT_SHIM_MODE is not set, auto-detects:
//   - If session registry exists → operator mode
//   - Otherwise → agent mode
//
// Usage: Place at /usr/local/bin/tmux in all containers.
// Set GT_REAL_TMUX=/usr/bin/tmux to point to the real tmux binary.

const registryPath = "/var/run/gt-operator/session-registry.json"

func main() {
	os.Setenv("GT_TMUX_SHIM", "1")

	mode := detectMode()
	namespace := os.Getenv("GT_NAMESPACE")
	if namespace == "" {
		namespace = "gastown"
	}

	s := shim.NewShim(mode, namespace)

	if mode == shim.ModeOperator {
		// Operator mode: load session registry for pod routing
		router := shim.NewMapRouter()
		// TODO: Load registry from shared file or operator API
		s.Router = router
	}

	if err := s.Exec(os.Args[1:]); err != nil {
		// If the error is from a remote nudge queue write, exit 0
		// to avoid breaking the caller's flow — the nudge will be
		// processed asynchronously by the operator.
		if mode == shim.ModeAgent {
			fmt.Fprintf(os.Stderr, "tmux-shim: queued for remote execution: %v\n", err)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "tmux-shim: %v\n", err)
		os.Exit(1)
	}
}

func detectMode() shim.Mode {
	switch os.Getenv("GT_SHIM_MODE") {
	case "operator":
		return shim.ModeOperator
	case "agent":
		return shim.ModeAgent
	}

	// Auto-detect: operator has the registry file
	if _, err := os.Stat(registryPath); err == nil {
		return shim.ModeOperator
	}
	return shim.ModeAgent
}
