package controller

// PolecatReconciler watches Polecat CRDs and reconciles them into Pods.
//
// When a Polecat CRD is created (by gt sling):
//   1. Create a Pod with the universal agent image
//   2. Set env vars: GT_ROLE, GT_RIG, GT_POLECAT, GT_BRANCH, GT_RUNTIME=k8s
//   3. Mount the rig's RWX PVC at /gt/<rigname>
//   4. Mount Dolt connection config from ConfigMap
//   5. Mount Claude credentials from Secret
//   6. Pod entrypoint creates worktree + starts tmux + launches agent
//
// When a Polecat CRD is deleted (by gt nuke):
//   1. Finalizer runs: clean up worktree on PVC, update bead state in Dolt
//   2. Delete the Pod
//
// When a Pod dies unexpectedly:
//   1. Operator detects via informer
//   2. Recreates the Pod (worktree still on PVC)
//   3. Agent resumes from hook state in Dolt
type PolecatReconciler struct {
	// TODO: k8s client, namespace, image, PVC names
}

// Reconcile ensures the desired Polecat state matches actual Pod state.
func (r *PolecatReconciler) Reconcile() {
	// TODO: List Polecat CRDs → compare with running Pods → create/delete as needed
}
