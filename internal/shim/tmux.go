package shim

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TmuxShim intercepts tmux commands and routes them to the correct target.
//
// The shim is placed at /usr/local/bin/tmux in all Gas Town containers
// (operator and agent pods), ahead of /usr/bin/tmux in PATH.
//
// Behavior depends on the pod type:
//
// Operator pod (has kubectl + session registry):
//   - Local session → passthrough to real tmux
//   - Remote session → kubectl exec into target pod
//
// Agent pod (no kubectl, no registry):
//   - Local session → passthrough to real tmux
//   - Remote session → write nudge request to Dolt queue
//     (operator daemon polls the queue and executes via kubectl exec)

// SessionRouter maps tmux session names to pod names.
type SessionRouter interface {
	// PodForSession returns the pod name for a tmux session, or empty if local.
	PodForSession(sessionName string) string
}

// NudgeWriter queues tmux commands for remote execution by the operator.
type NudgeWriter interface {
	// QueueTmuxCommand writes a tmux command to the nudge queue in Dolt.
	// The operator daemon will pick it up and execute via kubectl exec.
	QueueTmuxCommand(sessionName string, args []string) error
}

// Mode determines how the shim handles remote sessions.
type Mode int

const (
	// ModeOperator routes remote sessions via kubectl exec (operator pod).
	ModeOperator Mode = iota
	// ModeAgent routes remote sessions via Dolt nudge queue (agent pods).
	ModeAgent
)

// Shim wraps tmux calls with routing for cross-pod tmux operations.
type Shim struct {
	Mode        Mode
	Router      SessionRouter // Used in ModeOperator
	NudgeWriter NudgeWriter   // Used in ModeAgent
	Namespace   string
	RealTmux    string // Path to real tmux binary, e.g. /usr/bin/tmux
}

// NewShim creates a tmux shim. Mode is determined by GT_SHIM_MODE env var:
// "operator" (default if session registry exists) or "agent".
func NewShim(mode Mode, namespace string) *Shim {
	realTmux := "/usr/bin/tmux"
	if path := os.Getenv("GT_REAL_TMUX"); path != "" {
		realTmux = path
	}
	return &Shim{
		Mode:      mode,
		Namespace: namespace,
		RealTmux:  realTmux,
	}
}

// Exec processes a tmux command, routing based on mode and session location.
func (s *Shim) Exec(args []string) error {
	sessionName := extractSessionTarget(args)

	// No session target — always run locally (e.g. tmux ls, tmux new-session)
	if sessionName == "" {
		return s.execLocal(args)
	}

	// Check if the session exists locally first
	if s.hasLocalSession(sessionName) {
		return s.execLocal(args)
	}

	// Session is not local — route based on mode
	switch s.Mode {
	case ModeOperator:
		return s.execViaKubectl(sessionName, args)
	case ModeAgent:
		return s.execViaNudgeQueue(sessionName, args)
	default:
		return s.execLocal(args)
	}
}

// hasLocalSession checks if a tmux session exists on this host.
func (s *Shim) hasLocalSession(name string) bool {
	cmd := exec.Command(s.RealTmux, "has-session", "-t", name)
	return cmd.Run() == nil
}

// execLocal runs tmux directly on this host.
func (s *Shim) execLocal(args []string) error {
	cmd := exec.Command(s.RealTmux, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execViaKubectl runs tmux inside a remote pod via kubectl exec.
// Only available in ModeOperator (operator pod has kubectl + RBAC).
func (s *Shim) execViaKubectl(sessionName string, args []string) error {
	if s.Router == nil {
		return fmt.Errorf("no session router configured")
	}

	podName := s.Router.PodForSession(sessionName)
	if podName == "" {
		return fmt.Errorf("no pod found for session %q", sessionName)
	}

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

// execViaNudgeQueue writes the tmux command to the Dolt nudge queue.
// The operator daemon polls this queue and executes via kubectl exec.
// Only used in ModeAgent (agent pods without kubectl access).
func (s *Shim) execViaNudgeQueue(sessionName string, args []string) error {
	if s.NudgeWriter != nil {
		return s.NudgeWriter.QueueTmuxCommand(sessionName, args)
	}

	// Fallback: write to filesystem queue on shared PVC
	return s.writeNudgeFile(sessionName, args)
}

// NudgeRequest represents a queued tmux command for the operator to execute.
type NudgeRequest struct {
	SessionName string    `json:"session_name"`
	Args        []string  `json:"args"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"created_at"`
}

// writeNudgeFile writes a nudge request to the shared PVC as a JSON file.
// The operator daemon watches this directory and processes requests.
func (s *Shim) writeNudgeFile(sessionName string, args []string) error {
	queueDir := os.Getenv("GT_NUDGE_QUEUE_DIR")
	if queueDir == "" {
		queueDir = "/gt/.runtime/nudge-queue"
	}

	if err := os.MkdirAll(queueDir, 0755); err != nil {
		return fmt.Errorf("failed to create nudge queue dir: %w", err)
	}

	req := NudgeRequest{
		SessionName: sessionName,
		Args:        args,
		Source:      os.Getenv("GT_ROLE"),
		CreatedAt:   time.Now().UTC(),
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal nudge request: %w", err)
	}

	// Use timestamp + random suffix for unique filename
	filename := fmt.Sprintf("%d-%s.json", time.Now().UnixNano(), sessionName)
	path := filepath.Join(queueDir, filename)

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write nudge request: %w", err)
	}

	return nil
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

// --- Session Registry (operator-side) ---

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

// PodForSession returns the pod for a session, or empty if not found.
func (m *MapSessionToPod) PodForSession(sessionName string) string {
	return m.mapping[sessionName]
}
