package controller

import (
	"context"
	"fmt"
	"log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// RigReconciler watches Rig CRDs and ensures per-rig infrastructure is running.
//
// When a Rig CRD is created:
//  1. Create the RWX PVC for the rig (bare repo, worktrees, locks)
//  2. Create Witness Deployment (1 replica)
//  3. Create Refinery Deployment (1 replica)
//
// When a Rig CRD is deleted:
//  1. Delete Witness and Refinery Deployments
//  2. Optionally clean up the PVC (configurable: retain or delete)
type RigReconciler struct {
	clientset  kubernetes.Interface
	dynClient  dynamic.Interface
	namespace  string
	agentImage string
}

// Reconcile ensures per-rig infrastructure matches the Rig CRD spec.
func (r *RigReconciler) Reconcile(ctx context.Context, rig *unstructured.Unstructured) {
	name := rig.GetName()
	spec, ok := rig.Object["spec"].(map[string]interface{})
	if !ok {
		log.Printf("[rig] %s: missing spec", name)
		return
	}

	rigName, _ := spec["name"].(string)
	if rigName == "" {
		rigName = name
	}
	gitURL, _ := spec["gitUrl"].(string)
	storageClass, _ := spec["storageClass"].(string)
	storageSize, _ := spec["storageSize"].(string)
	if storageSize == "" {
		storageSize = "50Gi"
	}

	if gitURL == "" {
		log.Printf("[rig] %s: gitUrl is required", name)
		r.setStatus(ctx, rig, "Error", "", false, false, "missing required spec field: gitUrl")
		return
	}

	pvcName := fmt.Sprintf("rig-%s", rigName)
	rigMountPath := fmt.Sprintf("/gt/%s", rigName)

	// Ensure RWX PVC exists
	if err := r.ensurePVC(ctx, pvcName, storageClass, storageSize); err != nil {
		log.Printf("[rig] %s: failed to ensure PVC: %v", name, err)
		r.setStatus(ctx, rig, "Error", pvcName, false, false, fmt.Sprintf("PVC creation failed: %v", err))
		return
	}

	// Ensure Witness Deployment
	witnessReady, err := r.ensureDeployment(ctx, rigName, "witness", pvcName, rigMountPath, gitURL)
	if err != nil {
		log.Printf("[rig] %s: failed to ensure witness: %v", name, err)
	}

	// Ensure Refinery Deployment
	refineryReady, err := r.ensureDeployment(ctx, rigName, "refinery", pvcName, rigMountPath, gitURL)
	if err != nil {
		log.Printf("[rig] %s: failed to ensure refinery: %v", name, err)
	}

	phase := "Initializing"
	if witnessReady && refineryReady {
		phase = "Ready"
	}

	r.setStatus(ctx, rig, phase, pvcName, witnessReady, refineryReady, "")
	log.Printf("[rig] %s: reconciled (pvc=%s, witness=%v, refinery=%v)", name, pvcName, witnessReady, refineryReady)
}

// HandleDelete cleans up rig infrastructure when a Rig CRD is deleted.
func (r *RigReconciler) HandleDelete(ctx context.Context, rig *unstructured.Unstructured) {
	name := rig.GetName()
	spec, _ := rig.Object["spec"].(map[string]interface{})
	rigName, _ := spec["name"].(string)
	if rigName == "" {
		rigName = name
	}

	// Delete witness and refinery deployments
	for _, role := range []string{"witness", "refinery"} {
		depName := fmt.Sprintf("%s-%s", rigName, role)
		err := r.clientset.AppsV1().Deployments(r.namespace).Delete(ctx, depName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			log.Printf("[rig] %s: failed to delete %s deployment: %v", name, role, err)
		} else {
			log.Printf("[rig] %s: deleted %s deployment", name, role)
		}
	}

	// PVC is retained by default (data preservation)
	log.Printf("[rig] %s: cleanup complete (PVC retained)", name)
}

// ensurePVC creates the RWX PVC if it doesn't exist.
func (r *RigReconciler) ensurePVC(ctx context.Context, pvcName, storageClass, storageSize string) error {
	_, err := r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err == nil {
		return nil // Already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	accessModes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "rig",
				"app.kubernetes.io/component": "storage",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: accessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(storageSize),
				},
			},
		},
	}

	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}

	_, err = r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create PVC %s: %w", pvcName, err)
	}

	log.Printf("[rig] created PVC %s (%s, class=%s)", pvcName, storageSize, storageClass)
	return nil
}

// ensureDeployment creates or verifies a witness/refinery deployment for a rig.
// Returns true if the deployment has at least one ready replica.
func (r *RigReconciler) ensureDeployment(ctx context.Context, rigName, role, pvcName, rigMountPath, gitURL string) (bool, error) {
	depName := fmt.Sprintf("%s-%s", rigName, role)

	existing, err := r.clientset.AppsV1().Deployments(r.namespace).Get(ctx, depName, metav1.GetOptions{})
	if err == nil {
		return existing.Status.ReadyReplicas > 0, nil
	}
	if !errors.IsNotFound(err) {
		return false, err
	}

	replicas := int32(1)
	labels := map[string]string{
		"app.kubernetes.io/name":      role,
		"app.kubernetes.io/instance":  depName,
		"app.kubernetes.io/component": role,
		"gastown.io/rig":              rigName,
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      depName,
			Namespace: r.namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  role,
							Image: r.agentImage,
							Env: []corev1.EnvVar{
								{Name: "GT_RUNTIME", Value: "k8s"},
								{Name: "GT_RIG", Value: rigName},
								{Name: "GT_ROLE", Value: fmt.Sprintf("%s/%s", rigName, role)},
								{Name: "GT_GIT_URL", Value: gitURL},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "rig-data", MountPath: rigMountPath},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "rig-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err = r.clientset.AppsV1().Deployments(r.namespace).Create(ctx, dep, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create deployment %s: %w", depName, err)
	}

	log.Printf("[rig] created %s deployment %s", role, depName)
	return false, nil // Just created, not ready yet
}

// setStatus updates the Rig CRD status subresource.
func (r *RigReconciler) setStatus(ctx context.Context, rig *unstructured.Unstructured, phase, pvcName string, witnessReady, refineryReady bool, message string) {
	status := map[string]interface{}{
		"phase":          phase,
		"witnessReady":   witnessReady,
		"refineryReady":  refineryReady,
	}
	if pvcName != "" {
		status["pvcName"] = pvcName
	}
	if message != "" {
		status["message"] = message
	}

	patch := rig.DeepCopy()
	if err := unstructured.SetNestedMap(patch.Object, status, "status"); err != nil {
		log.Printf("[rig] %s: failed to build status patch: %v", rig.GetName(), err)
		return
	}

	_, err := r.dynClient.Resource(rigGVR).Namespace(r.namespace).UpdateStatus(ctx, patch, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("[rig] %s: failed to update status: %v", rig.GetName(), err)
	}
}
