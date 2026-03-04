package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/anthropics/gt-operator/internal/shim"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	// polecatFinalizer is added to Polecat CRDs to ensure worktree cleanup
	// runs before the resource is removed from the API server.
	polecatFinalizer = "gastown.io/worktree-cleanup"
)

// PolecatReconciler watches Polecat CRDs and reconciles them into Pods.
//
// When a Polecat CRD is created (by gt sling):
//  1. Add the worktree-cleanup finalizer
//  2. Create a Pod with the universal agent image
//  3. Set env vars: GT_ROLE, GT_RIG, GT_POLECAT, GT_BRANCH, GT_RUNTIME=k8s
//  4. Mount the rig's RWX PVC at /gt/<rigname>
//  5. Mount Dolt connection config from ConfigMap
//  6. Mount Claude credentials from Secret
//  7. Pod entrypoint creates worktree + starts tmux + launches agent
//
// When a Polecat CRD is deleted (by gt nuke):
//  1. Finalizer runs: clean up worktree on PVC
//  2. Delete the Pod and unregister session
//  3. Remove the finalizer so Kubernetes completes deletion
//
// When a Pod dies unexpectedly:
//  1. Operator detects via informer
//  2. Recreates the Pod (worktree still on PVC)
//  3. Agent resumes from hook state in Dolt
type PolecatReconciler struct {
	clientset     kubernetes.Interface
	dynClient     dynamic.Interface
	namespace     string
	agentImage    string
	sessionRouter *shim.MapSessionToPod
}

// Reconcile ensures the desired Polecat state matches actual Pod state.
// It implements the Kubernetes finalizer pattern: when deletionTimestamp is set,
// it performs worktree cleanup before allowing the resource to be removed.
func (r *PolecatReconciler) Reconcile(ctx context.Context, polecat *unstructured.Unstructured) {
	name := polecat.GetName()

	// Check if the resource is being deleted (has a deletionTimestamp).
	// With finalizers, Kubernetes sets deletionTimestamp but won't remove
	// the resource until all finalizers are cleared.
	if polecat.GetDeletionTimestamp() != nil {
		r.handleFinalizerCleanup(ctx, polecat)
		return
	}

	spec, ok := polecat.Object["spec"].(map[string]interface{})
	if !ok {
		log.Printf("[polecat] %s: missing spec", name)
		return
	}

	rigName, _ := spec["rig"].(string)
	beadID, _ := spec["bead"].(string)
	branch, _ := spec["branch"].(string)
	formula, _ := spec["formula"].(string)

	if rigName == "" || beadID == "" {
		log.Printf("[polecat] %s: rig and bead are required", name)
		r.setStatus(ctx, polecat, "Failed", "", "missing required spec fields: rig, bead")
		return
	}

	// Ensure the finalizer is present before creating any resources.
	if !hasFinalizer(polecat, polecatFinalizer) {
		if err := r.addFinalizer(ctx, polecat); err != nil {
			log.Printf("[polecat] %s: failed to add finalizer: %v", name, err)
			return
		}
	}

	podName := fmt.Sprintf("polecat-%s", name)

	// Check if pod already exists
	_, err := r.clientset.CoreV1().Pods(r.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		// Pod exists, nothing to do
		return
	}
	if !errors.IsNotFound(err) {
		log.Printf("[polecat] %s: error checking pod: %v", name, err)
		return
	}

	// Build the pod
	pod := r.buildPod(podName, name, rigName, beadID, branch, formula)

	_, err = r.clientset.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		log.Printf("[polecat] %s: failed to create pod: %v", name, err)
		r.setStatus(ctx, polecat, "Failed", podName, fmt.Sprintf("pod creation failed: %v", err))
		return
	}

	log.Printf("[polecat] %s: created pod %s (rig=%s, bead=%s)", name, podName, rigName, beadID)

	// Register session in the router for cross-pod tmux
	sessionName := fmt.Sprintf("gt-%s", name)
	r.sessionRouter.Register(sessionName, podName)

	r.setStatus(ctx, polecat, "Pending", podName, "pod created")
}

// handleFinalizerCleanup runs when the Polecat CRD has a deletionTimestamp.
// It cleans up the worktree, deletes the pod, and removes the finalizer
// so Kubernetes can complete the deletion.
func (r *PolecatReconciler) handleFinalizerCleanup(ctx context.Context, polecat *unstructured.Unstructured) {
	name := polecat.GetName()

	if !hasFinalizer(polecat, polecatFinalizer) {
		return
	}

	spec, _ := polecat.Object["spec"].(map[string]interface{})
	rigName, _ := spec["rig"].(string)

	// Clean up the worktree on the rig PVC
	if rigName != "" {
		r.cleanupWorktree(name, rigName)
	}

	// Delete the pod
	podName := fmt.Sprintf("polecat-%s", name)
	err := r.clientset.CoreV1().Pods(r.namespace).Delete(ctx, podName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		log.Printf("[polecat] %s: failed to delete pod during finalizer: %v", name, err)
	}

	// Unregister session from router
	sessionName := fmt.Sprintf("gt-%s", name)
	r.sessionRouter.Unregister(sessionName)

	// Remove the finalizer to allow Kubernetes to complete deletion
	if err := r.removeFinalizer(ctx, polecat); err != nil {
		log.Printf("[polecat] %s: failed to remove finalizer: %v", name, err)
		return
	}

	log.Printf("[polecat] %s: finalizer cleanup complete (worktree removed, pod deleted)", name)
}

// HandleDelete is a safety net for the informer DeleteFunc.
// With finalizers, primary cleanup happens in handleFinalizerCleanup via Reconcile.
// This handles edge cases where the finalizer was removed manually.
func (r *PolecatReconciler) HandleDelete(ctx context.Context, polecat *unstructured.Unstructured) {
	name := polecat.GetName()
	podName := fmt.Sprintf("polecat-%s", name)

	// Unregister session from router
	sessionName := fmt.Sprintf("gt-%s", name)
	r.sessionRouter.Unregister(sessionName)

	// Delete the pod (ignore not-found)
	err := r.clientset.CoreV1().Pods(r.namespace).Delete(ctx, podName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		log.Printf("[polecat] %s: failed to delete pod: %v", name, err)
		return
	}

	log.Printf("[polecat] %s: deleted pod %s", name, podName)
}

// cleanupWorktree removes the polecat's worktree directory from the rig PVC.
// The worktree lives at /gt/<rigname>/polecats/<polecatname>/ on the shared RWX PVC.
func (r *PolecatReconciler) cleanupWorktree(polecatName, rigName string) {
	worktreePath := filepath.Join("/gt", rigName, "polecats", polecatName)

	info, err := os.Stat(worktreePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[polecat] %s: worktree already removed (%s)", polecatName, worktreePath)
			return
		}
		log.Printf("[polecat] %s: failed to stat worktree %s: %v", polecatName, worktreePath, err)
		return
	}
	if !info.IsDir() {
		log.Printf("[polecat] %s: worktree path is not a directory: %s", polecatName, worktreePath)
		return
	}

	if err := os.RemoveAll(worktreePath); err != nil {
		log.Printf("[polecat] %s: failed to remove worktree %s: %v", polecatName, worktreePath, err)
		return
	}

	log.Printf("[polecat] %s: removed worktree %s", polecatName, worktreePath)
}

// addFinalizer adds the worktree-cleanup finalizer to the Polecat CRD.
func (r *PolecatReconciler) addFinalizer(ctx context.Context, polecat *unstructured.Unstructured) error {
	finalizers := polecat.GetFinalizers()
	finalizers = append(finalizers, polecatFinalizer)

	patch := polecat.DeepCopy()
	patch.SetFinalizers(finalizers)

	_, err := r.dynClient.Resource(polecatGVR).Namespace(r.namespace).Update(ctx, patch, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update finalizers: %w", err)
	}

	log.Printf("[polecat] %s: added finalizer %s", polecat.GetName(), polecatFinalizer)
	return nil
}

// removeFinalizer removes the worktree-cleanup finalizer from the Polecat CRD.
func (r *PolecatReconciler) removeFinalizer(ctx context.Context, polecat *unstructured.Unstructured) error {
	finalizers := polecat.GetFinalizers()
	var updated []string
	for _, f := range finalizers {
		if f != polecatFinalizer {
			updated = append(updated, f)
		}
	}

	patch := polecat.DeepCopy()
	patch.SetFinalizers(updated)

	_, err := r.dynClient.Resource(polecatGVR).Namespace(r.namespace).Update(ctx, patch, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update finalizers: %w", err)
	}

	log.Printf("[polecat] %s: removed finalizer %s", polecat.GetName(), polecatFinalizer)
	return nil
}

// hasFinalizer checks whether the resource has the given finalizer.
func hasFinalizer(obj *unstructured.Unstructured, finalizer string) bool {
	for _, f := range obj.GetFinalizers() {
		if f == finalizer {
			return true
		}
	}
	return false
}

// buildPod constructs the Pod spec for a polecat agent.
func (r *PolecatReconciler) buildPod(podName, polecatName, rigName, beadID, branch, formula string) *corev1.Pod {
	labels := map[string]string{
		"app.kubernetes.io/name":      "polecat",
		"app.kubernetes.io/instance":  polecatName,
		"app.kubernetes.io/component": "agent",
		"gastown.io/rig":              rigName,
		"gastown.io/polecat":          polecatName,
	}

	env := []corev1.EnvVar{
		{Name: "GT_RUNTIME", Value: "k8s"},
		{Name: "GT_RIG", Value: rigName},
		{Name: "GT_POLECAT", Value: polecatName},
		{Name: "GT_ROLE", Value: fmt.Sprintf("%s/polecats/%s", rigName, polecatName)},
		{Name: "GT_BEAD", Value: beadID},
	}
	if branch != "" {
		env = append(env, corev1.EnvVar{Name: "GT_BRANCH", Value: branch})
	}
	if formula != "" {
		env = append(env, corev1.EnvVar{Name: "GT_FORMULA", Value: formula})
	}

	rigPVCName := fmt.Sprintf("rig-%s", rigName)
	rigMountPath := fmt.Sprintf("/gt/%s", rigName)

	volumes := []corev1.Volume{
		{
			Name: "rig-data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: rigPVCName,
				},
			},
		},
		{
			Name: "dolt-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "dolt-config",
					},
				},
			},
		},
		{
			Name: "claude-credentials",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "claude-credentials",
					Optional:   boolPtr(true),
				},
			},
		},
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "rig-data", MountPath: rigMountPath},
		{Name: "dolt-config", MountPath: "/etc/gt/dolt", ReadOnly: true},
		{Name: "claude-credentials", MountPath: "/etc/gt/claude", ReadOnly: true},
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: r.namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:         "agent",
					Image:        r.agentImage,
					Env:          env,
					VolumeMounts: volumeMounts,
				},
			},
			Volumes: volumes,
		},
	}
}

// setStatus updates the Polecat CRD status subresource.
func (r *PolecatReconciler) setStatus(ctx context.Context, polecat *unstructured.Unstructured, phase, podName, message string) {
	status := map[string]interface{}{
		"phase":   phase,
		"message": message,
	}
	if podName != "" {
		status["podName"] = podName
		status["sessionName"] = fmt.Sprintf("gt-%s", polecat.GetName())
	}

	patch := polecat.DeepCopy()
	if err := unstructured.SetNestedMap(patch.Object, status, "status"); err != nil {
		log.Printf("[polecat] %s: failed to build status patch: %v", polecat.GetName(), err)
		return
	}

	_, err := r.dynClient.Resource(polecatGVR).Namespace(r.namespace).UpdateStatus(ctx, patch, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("[polecat] %s: failed to update status: %v", polecat.GetName(), err)
	}
}

func boolPtr(b bool) *bool { return &b }
