package controller

// NudgeProcessor watches the nudge queue on the shared PVC and executes
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
	// TODO: session registry, namespace, poll interval
}

// Run starts the nudge queue processor loop.
func (p *NudgeProcessor) Run() {
	// TODO:
	// 1. List all rig PVC mount points
	// 2. For each: watch <mount>/.runtime/nudge-queue/ for new files
	// 3. Parse NudgeRequest JSON
	// 4. Look up session → pod via session registry
	// 5. kubectl exec <pod> -- /usr/bin/tmux <args>
	// 6. Delete the request file on success
	// 7. On failure: retry with backoff, move to dead-letter after N attempts
}
