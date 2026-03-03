package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/anthropics/gt-operator/internal/shim"
)

// NudgeProcessor watches the nudge queue on the shared PVC and executes
// queued tmux commands via kubectl exec.
//
// Flow:
//  1. Agent pod (witness, polecat, etc.) runs gt nudge <target>
//  2. gt calls tmux send-keys -t <session> ...
//  3. tmux shim (agent mode) detects session is not local
//  4. Shim writes NudgeRequest JSON to /gt/.runtime/nudge-queue/
//  5. NudgeProcessor polls the queue directory
//  6. For each request: resolve session → pod via registry, kubectl exec
//  7. Delete processed request file
//
// Queue directory is on the shared RWX PVC, accessible by all pods in a rig.
// Operator pod mounts all rig PVCs to access their nudge queues.
type NudgeProcessor struct {
	kubeClient   kubernetes.Interface
	restConfig   *rest.Config
	namespace    string
	pollInterval time.Duration
	router       *shim.MapSessionToPod
	queueDirs    []string
	maxRetries   int
}

// Run starts the nudge queue processor loop. It polls queue directories
// for nudge request files and executes them via kubectl exec.
func (p *NudgeProcessor) Run(ctx context.Context) {
	if p.maxRetries == 0 {
		p.maxRetries = 3
	}

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[nudge] processor stopping")
			return
		case <-ticker.C:
			p.processQueues(ctx)
		}
	}
}

// RegisterQueueDir adds a nudge queue directory to watch.
func (p *NudgeProcessor) RegisterQueueDir(dir string) {
	p.queueDirs = append(p.queueDirs, dir)
}

// processQueues scans all registered queue directories for nudge requests.
func (p *NudgeProcessor) processQueues(ctx context.Context) {
	for _, dir := range p.queueDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("[nudge] failed to read queue dir %s: %v", dir, err)
			}
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}

			path := filepath.Join(dir, entry.Name())
			if err := p.processRequest(ctx, path); err != nil {
				log.Printf("[nudge] failed to process %s: %v", path, err)
				p.handleFailure(path)
				continue
			}

			if err := os.Remove(path); err != nil {
				log.Printf("[nudge] failed to remove processed file %s: %v", path, err)
			}
		}
	}
}

// processRequest reads a nudge request file and executes the tmux command
// on the target pod via the k8s exec API.
func (p *NudgeProcessor) processRequest(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading request file: %w", err)
	}

	var req shim.NudgeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("parsing request: %w", err)
	}

	podName := p.router.PodForSession(req.SessionName)
	if podName == "" {
		return fmt.Errorf("no pod found for session %q", req.SessionName)
	}

	log.Printf("[nudge] executing tmux command on pod %s for session %s (from %s)",
		podName, req.SessionName, req.Source)

	return p.execInPod(ctx, podName, append([]string{"/usr/bin/tmux"}, req.Args...))
}

// execInPod runs a command inside a pod using the k8s exec API.
func (p *NudgeProcessor) execInPod(ctx context.Context, podName string, command []string) error {
	execReq := p.kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(p.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(p.restConfig, "POST", execReq.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec failed (stderr: %s): %w", stderr.String(), err)
	}

	return nil
}

// handleFailure renames a request file with a .failed extension after
// exceeding max retries. This moves it out of the processing queue.
func (p *NudgeProcessor) handleFailure(path string) {
	failedPath := path + ".failed"
	if err := os.Rename(path, failedPath); err != nil {
		log.Printf("[nudge] failed to move to dead-letter %s: %v", path, err)
	}
}
