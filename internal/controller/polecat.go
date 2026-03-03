package controller

import (
	"context"
	"fmt"
	"log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// PolecatReconciler watches Polecat CRDs and reconciles them into Pods.
//
// When a Polecat CRD is created (by gt sling):
//  1. Create a Pod with the universal agent image
//  2. Set env vars: GT_ROLE, GT_RIG, GT_POLECAT, GT_BRANCH, GT_RUNTIME=k8s
//  3. Mount the rig's RWX PVC at /gt/<rigname>
//  4. Mount Dolt connection config from ConfigMap
//  5. Mount Claude credentials from Secret
//  6. Pod entrypoint creates worktree + starts tmux + launches agent
//
// When a Polecat CRD is deleted (by gt nuke):
//  1. Finalizer runs: clean up worktree on PVC, update bead state in Dolt
//  2. Delete the Pod
//
// When a Pod dies unexpectedly:
//  1. Operator detects via informer
//  2. Recreates the Pod (worktree still on PVC)
//  3. Agent resumes from hook state in Dolt
type PolecatReconciler struct {
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface
	namespace     string
	agentImage    string
}

// Reconcile ensures the desired Polecat state matches actual Pod state.
// It lists all Polecat CRDs, compares with running Pods, and creates or
// deletes Pods as needed.
func (r *PolecatReconciler) Reconcile(ctx context.Context) error {
	// List all Polecat CRDs in the namespace.
	polecatList, err := r.dynamicClient.Resource(polecatGVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing polecats: %w", err)
	}

	// List all Pods managed by the operator.
	pods, err := r.kubeClient.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=gt-operator,app.kubernetes.io/component=polecat",
	})
	if err != nil {
		return fmt.Errorf("listing polecat pods: %w", err)
	}

	// Build a set of existing pod names for quick lookup.
	existingPods := make(map[string]corev1.Pod, len(pods.Items))
	for _, pod := range pods.Items {
		existingPods[pod.Name] = pod
	}

	// Build a set of desired polecat names.
	desiredPolecats := make(map[string]*unstructured.Unstructured, len(polecatList.Items))
	for i := range polecatList.Items {
		pc := &polecatList.Items[i]
		desiredPolecats[pc.GetName()] = pc
	}

	// Create pods for polecats that don't have one.
	for name, pc := range desiredPolecats {
		podName := "polecat-" + name
		if _, exists := existingPods[podName]; exists {
			continue
		}

		log.Printf("[polecat] creating pod %s for CRD %s", podName, name)
		pod := r.buildPod(podName, pc)
		if _, err := r.kubeClient.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			log.Printf("[polecat] failed to create pod %s: %v", podName, err)
			r.updateStatus(ctx, pc, "Failed", podName, fmt.Sprintf("pod creation failed: %v", err))
			continue
		}
		r.updateStatus(ctx, pc, "Running", podName, "")
	}

	// Delete pods whose CRD no longer exists.
	for podName := range existingPods {
		// Pod names are "polecat-<crd-name>".
		crdName := podName[len("polecat-"):]
		if _, exists := desiredPolecats[crdName]; exists {
			continue
		}

		log.Printf("[polecat] deleting orphan pod %s (CRD %s removed)", podName, crdName)
		if err := r.kubeClient.CoreV1().Pods(r.namespace).Delete(ctx, podName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			log.Printf("[polecat] failed to delete pod %s: %v", podName, err)
		}
	}

	return nil
}

// buildPod constructs the Pod spec for a Polecat.
func (r *PolecatReconciler) buildPod(podName string, pc *unstructured.Unstructured) *corev1.Pod {
	spec, _ := pc.Object["spec"].(map[string]interface{})
	rigName, _ := spec["rig"].(string)
	beadID, _ := spec["bead"].(string)
	branch, _ := spec["branch"].(string)
	pcName := pc.GetName()

	pvcName := "rig-" + rigName

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gt-operator",
				"app.kubernetes.io/component":  "polecat",
				"gastown.io/rig":               rigName,
				"gastown.io/polecat":           pcName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "gastown.io/v1",
					Kind:       "Polecat",
					Name:       pc.GetName(),
					UID:        pc.GetUID(),
				},
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "agent",
					Image: r.agentImage,
					Env: []corev1.EnvVar{
						{Name: "GT_RUNTIME", Value: "k8s"},
						{Name: "GT_RIG", Value: rigName},
						{Name: "GT_POLECAT", Value: pcName},
						{Name: "GT_ROLE", Value: rigName + "/polecats/" + pcName},
						{Name: "GT_BRANCH", Value: branch},
						{Name: "GT_BEAD", Value: beadID},
						{Name: "GT_NAMESPACE", Value: r.namespace},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "rig-storage",
							MountPath: "/gt/" + rigName,
						},
						{
							Name:      "claude-credentials",
							MountPath: "/etc/claude",
							ReadOnly:  true,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "rig-storage",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
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
			},
		},
	}
}

// updateStatus updates the status subresource of a Polecat CRD.
func (r *PolecatReconciler) updateStatus(ctx context.Context, pc *unstructured.Unstructured, phase, podName, message string) {
	status := map[string]interface{}{
		"phase":   phase,
		"podName": podName,
	}
	if message != "" {
		status["message"] = message
	}

	patch := pc.DeepCopy()
	patch.Object["status"] = status
	_, err := r.dynamicClient.Resource(polecatGVR).Namespace(r.namespace).UpdateStatus(ctx, patch, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("[polecat] failed to update status for %s: %v", pc.GetName(), err)
	}
}

func boolPtr(b bool) *bool {
	return &b
}
