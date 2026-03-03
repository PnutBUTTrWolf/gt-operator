package shim

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// TmuxShim intercepts tmux commands and routes them to the correct target.
//
// For local sessions (daemon's own tmux): passthrough to real /usr/bin/tmux.
// For remote sessions (agent pods): translate to kubectl exec.
//
// The shim is placed at /usr/local/bin/tmux in the operator container,
// ahead of /usr/bin/tmux in PATH. When the gt daemon calls "tmux",
// it hits this shim first.
//
// Session-to-pod mapping is resolved via the Polecat CRD labels
// or a local registry maintained by the operator.

// SessionRouter maps tmux session names to pod names.
type SessionRouter interface {
	// PodForSession returns the pod name for a tmux session, or empty if local.
	PodForSession(sessionName string) string
}

// Shim wraps tmux calls with optional kubectl exec routing.
type Shim struct {
	Router    SessionRouter
	Namespace string
	RealTmux  string // Path to real tmux binary, e.g. /usr/bin/tmux
}

// NewShim creates a tmux shim.
func NewShim(router SessionRouter, namespace string) *Shim {
	realTmux := "/usr/bin/tmux"
	if path := os.Getenv("GT_REAL_TMUX"); path != "" {
		realTmux = path
	}
	return &Shim{
		Router:    router,
		Namespace: namespace,
		RealTmux:  realTmux,
	}
}

// Exec processes a tmux command, routing to kubectl exec if the target
// session lives in a remote pod.
func (s *Shim) Exec(args []string) error {
	sessionName := extractSessionTarget(args)
	if sessionName == "" {
		// No session target found — run locally
		return s.execLocal(args)
	}

	podName := s.Router.PodForSession(sessionName)
	if podName == "" {
		// Session is local to this pod
		return s.execLocal(args)
	}

	// Session is in a remote pod — route via kubectl exec
	return s.execRemote(podName, args)
}

// execLocal runs tmux directly on this host.
func (s *Shim) execLocal(args []string) error {
	cmd := exec.Command(s.RealTmux, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execRemote runs tmux inside a remote pod via kubectl exec.
func (s *Shim) execRemote(podName string, args []string) error {
	kubectlArgs := []string{
		"exec", podName,
		"-n", s.Namespace,
		"--", s.RealTmux,
	}
	kubectlArgs = append(kubectlArgs, args...)

	cmd := exec.Command("kubectl", kubectlArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// extractSessionTarget parses tmux args to find the -t <session> target.
// Handles: tmux send-keys -t <session> ..., tmux has-session -t <session>, etc.
func extractSessionTarget(args []string) string {
	for i, arg := range args {
		if arg == "-t" && i+1 < len(args) {
			target := args[i+1]
			// Strip pane/window suffix (e.g., "gt-Toast:0.0" → "gt-Toast")
			if idx := strings.IndexByte(target, ':'); idx >= 0 {
				target = target[:idx]
			}
			return target
		}
		// Handle -t<session> (no space)
		if strings.HasPrefix(arg, "-t") && len(arg) > 2 {
			target := arg[2:]
			if idx := strings.IndexByte(target, ':'); idx >= 0 {
				target = target[:idx]
			}
			return target
		}
	}
	return ""
}

// MapSessionToPod provides a simple in-memory session→pod mapping.
// The operator updates this map as pods are created/deleted.
type MapSessionToPod struct {
	mapping map[string]string // session name → pod name
}

// NewMapRouter creates a session router with an initial mapping.
func NewMapRouter() *MapSessionToPod {
	return &MapSessionToPod{mapping: make(map[string]string)}
}

// Register adds a session→pod mapping.
func (m *MapSessionToPod) Register(sessionName, podName string) {
	m.mapping[sessionName] = podName
}

// Unregister removes a session mapping.
func (m *MapSessionToPod) Unregister(sessionName string) {
	delete(m.mapping, sessionName)
}

// PodForSession returns the pod for a session, or empty if local.
func (m *MapSessionToPod) PodForSession(sessionName string) string {
	pod, ok := m.mapping[sessionName]
	if !ok {
		return ""
	}
	return pod
}

func init() {
	// Prevent recursive shim calls — if GT_REAL_TMUX is set,
	// we're already inside the shim.
	if os.Getenv("GT_TMUX_SHIM") == "1" {
		fmt.Fprintf(os.Stderr, "tmux-shim: recursive call detected, using real tmux\n")
	}
}
