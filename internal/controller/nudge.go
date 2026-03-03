package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/gt-operator/internal/shim"
)

// NudgeProcessor watches the nudge queue on shared PVCs and executes
// queued tmux commands via kubectl exec.
//
// Flow:
//   1. Agent pod (witness, polecat, etc.) runs gt nudge <target>
//   2. gt calls tmux send-keys -t <session> ...
//   3. tmux shim (agent mode) detects session is not local
//   4. Shim writes NudgeRequest JSON to /gt/.runtime/nudge-queue/
//   5. NudgeProcessor polls the queue directory
//   6. For each request: resolve session → pod via registry, kubectl exec
//   7. Delete processed request file
//
// Queue directory is on the shared RWX PVC, accessible by all pods in a rig.
// Operator pod mounts all rig PVCs to access their nudge queues.
type NudgeProcessor struct {
	Router       shim.SessionRouter
	Namespace    string
	RealTmux     string
	RigMounts    []string // PVC mount points, e.g. ["/gt/my-rig", "/gt/other-rig"]
	PollInterval time.Duration
	MaxRetries   int
}

// NudgeProcessorConfig holds configuration for creating a NudgeProcessor.
type NudgeProcessorConfig struct {
	Router       shim.SessionRouter
	Namespace    string
	RigMounts    []string
	PollInterval time.Duration // Defaults to 2s if zero.
	MaxRetries   int           // Defaults to 3 if zero.
}

// NewNudgeProcessor creates a configured NudgeProcessor.
func NewNudgeProcessor(cfg NudgeProcessorConfig) *NudgeProcessor {
	pollInterval := cfg.PollInterval
	if pollInterval == 0 {
		pollInterval = 2 * time.Second
	}
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	realTmux := "/usr/bin/tmux"
	if path := os.Getenv("GT_REAL_TMUX"); path != "" {
		realTmux = path
	}
	return &NudgeProcessor{
		Router:       cfg.Router,
		Namespace:    cfg.Namespace,
		RealTmux:     realTmux,
		RigMounts:    cfg.RigMounts,
		PollInterval: pollInterval,
		MaxRetries:   maxRetries,
	}
}

// nudgeFile wraps a NudgeRequest with its filesystem path for processing.
type nudgeFile struct {
	Path    string
	Request shim.NudgeRequest
}

// Run starts the nudge queue processor loop. It blocks until ctx is cancelled.
func (p *NudgeProcessor) Run(ctx context.Context) error {
	log.Printf("[nudge] starting processor (poll=%s, retries=%d, rigs=%d)",
		p.PollInterval, p.MaxRetries, len(p.RigMounts))

	ticker := time.NewTicker(p.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[nudge] shutting down")
			return ctx.Err()
		case <-ticker.C:
			p.poll()
		}
	}
}

// poll scans all rig queue directories for pending nudge requests.
func (p *NudgeProcessor) poll() {
	for _, mount := range p.RigMounts {
		queueDir := filepath.Join(mount, ".runtime", "nudge-queue")
		p.processQueue(queueDir)
	}
}

// processQueue processes all JSON files in a single queue directory.
func (p *NudgeProcessor) processQueue(queueDir string) {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[nudge] error reading queue dir %s: %v", queueDir, err)
		}
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(queueDir, entry.Name())
		nf, err := p.parseNudgeFile(path)
		if err != nil {
			log.Printf("[nudge] failed to parse %s: %v", path, err)
			p.deadLetter(path, err)
			continue
		}

		if err := p.execute(nf); err != nil {
			log.Printf("[nudge] failed to execute %s: %v", path, err)
			if p.shouldDeadLetter(nf) {
				p.deadLetter(path, err)
			}
			continue
		}

		if err := os.Remove(path); err != nil {
			log.Printf("[nudge] failed to remove processed file %s: %v", path, err)
		}
	}
}

// parseNudgeFile reads and parses a nudge request JSON file.
func (p *NudgeProcessor) parseNudgeFile(path string) (*nudgeFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	var req shim.NudgeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	if req.SessionName == "" {
		return nil, fmt.Errorf("missing session_name")
	}

	return &nudgeFile{Path: path, Request: req}, nil
}

// execute resolves the session to a pod and runs the tmux command via kubectl exec.
func (p *NudgeProcessor) execute(nf *nudgeFile) error {
	podName := p.Router.PodForSession(nf.Request.SessionName)
	if podName == "" {
		return fmt.Errorf("no pod found for session %q", nf.Request.SessionName)
	}

	args := []string{
		"exec", podName,
		"-n", p.Namespace,
		"--", p.RealTmux,
	}
	args = append(args, nf.Request.Args...)

	cmd := exec.Command("kubectl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl exec: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	log.Printf("[nudge] executed: session=%s pod=%s source=%s",
		nf.Request.SessionName, podName, nf.Request.Source)
	return nil
}

// shouldDeadLetter checks if a nudge request has exceeded the retry window.
// Uses request age relative to MaxRetries * PollInterval as the threshold.
func (p *NudgeProcessor) shouldDeadLetter(nf *nudgeFile) bool {
	maxAge := time.Duration(p.MaxRetries) * p.PollInterval
	return time.Since(nf.Request.CreatedAt) > maxAge
}

// deadLetter moves a failed nudge file to the dead-letter subdirectory
// and writes a reason file alongside it.
func (p *NudgeProcessor) deadLetter(path string, reason error) {
	dlDir := filepath.Join(filepath.Dir(path), "dead-letter")
	if err := os.MkdirAll(dlDir, 0755); err != nil {
		log.Printf("[nudge] failed to create dead-letter dir: %v", err)
		os.Remove(path)
		return
	}

	dlPath := filepath.Join(dlDir, filepath.Base(path))
	if err := os.Rename(path, dlPath); err != nil {
		log.Printf("[nudge] failed to move to dead-letter: %v", err)
		os.Remove(path)
		return
	}

	reasonPath := dlPath + ".reason"
	os.WriteFile(reasonPath, []byte(reason.Error()), 0644)

	log.Printf("[nudge] dead-lettered: %s reason=%v", filepath.Base(path), reason)
}
