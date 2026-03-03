package controller

// RigReconciler watches Rig CRDs and ensures per-rig infrastructure is running.
//
// When a Rig CRD is created:
//   1. Create the RWX PVC for the rig (bare repo, worktrees, locks)
//   2. Create Witness Deployment (1 replica)
//   3. Create Refinery Deployment (1 replica)
//   4. Initialize the bare repo on the PVC (clone from git URL)
//
// When a Rig CRD is deleted:
//   1. Delete Witness and Refinery Deployments
//   2. Optionally clean up the PVC (configurable: retain or delete)
type RigReconciler struct {
	// TODO: k8s client, namespace, image, storage class
}

// Reconcile ensures per-rig infrastructure matches the Rig CRD spec.
func (r *RigReconciler) Reconcile() {
	// TODO: List Rig CRDs → ensure PVC + Witness + Refinery exist for each
}
