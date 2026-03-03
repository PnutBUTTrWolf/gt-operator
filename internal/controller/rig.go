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
//  4. Initialize the bare repo on the PVC (clone from git URL)
//
// When a Rig CRD is deleted:
//  1. Delete Witness and Refinery Deployments
//  2. Optionally clean up the PVC (configurable: retain or delete)
type RigReconciler struct {
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface
	namespace     string
	agentImage    string
}

// Reconcile ensures per-rig infrastructure matches the Rig CRD spec.
// It lists all Rig CRDs, then ensures each has a PVC, Witness, and Refinery.
func (r *RigReconciler) Reconcile(ctx context.Context) error {
	rigList, err := r.dynamicClient.Resource(rigGVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing rigs: %w", err)
	}

	for i := range rigList.Items {
		rig := &rigList.Items[i]
		rigName := rig.GetName()

		if err := r.reconcileRig(ctx, rig); err != nil {
			log.Printf("[rig] reconcile error for %s: %v", rigName, err)
			r.updateStatus(ctx, rig, "Error", err.Error())
			continue
		}
	}

	return nil
}

// reconcileRig ensures all infrastructure for a single rig exists.
func (r *RigReconciler) reconcileRig(ctx context.Context, rig *unstructured.Unstructured) error {
	rigName := rig.GetName()
	spec, _ := rig.Object["spec"].(map[string]interface{})

	storageSize := "50Gi"
	if sz, ok := spec["storageSize"].(string); ok && sz != "" {
		storageSize = sz
	}
	storageClass := ""
	if sc, ok := spec["storageClass"].(string); ok {
		storageClass = sc
	}

	// Ensure RWX PVC exists.
	pvcName := "rig-" + rigName
	if err := r.ensurePVC(ctx, pvcName, storageSize, storageClass, rig); err != nil {
		return fmt.Errorf("ensuring PVC: %w", err)
	}

	// Ensure Witness Deployment exists.
	if err := r.ensureDeployment(ctx, rigName, "witness", pvcName, rig); err != nil {
		return fmt.Errorf("ensuring witness: %w", err)
	}

	// Ensure Refinery Deployment exists.
	if err := r.ensureDeployment(ctx, rigName, "refinery", pvcName, rig); err != nil {
		return fmt.Errorf("ensuring refinery: %w", err)
	}

	// Update rig status to Ready.
	r.updateStatus(ctx, rig, "Ready", "")
	return nil
}

// ensurePVC creates the RWX PVC for a rig if it doesn't exist.
func (r *RigReconciler) ensurePVC(ctx context.Context, name, size, storageClass string, rig *unstructured.Unstructured) error {
	_, err := r.kubeClient.CoreV1().PersistentVolumeClaims(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	accessModes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gt-operator",
				"app.kubernetes.io/component":  "rig-storage",
				"gastown.io/rig":               rig.GetName(),
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "gastown.io/v1",
					Kind:       "Rig",
					Name:       rig.GetName(),
					UID:        rig.GetUID(),
				},
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: accessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}

	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}

	log.Printf("[rig] creating PVC %s (%s, RWX)", name, size)
	_, err = r.kubeClient.CoreV1().PersistentVolumeClaims(r.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	return err
}

// ensureDeployment creates a Witness or Refinery Deployment for a rig if it doesn't exist.
func (r *RigReconciler) ensureDeployment(ctx context.Context, rigName, component, pvcName string, rig *unstructured.Unstructured) error {
	deployName := rigName + "-" + component

	_, err := r.kubeClient.AppsV1().Deployments(r.namespace).Get(ctx, deployName, metav1.GetOptions{})
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	replicas := int32(1)
	role := rigName + "/" + component

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gt-operator",
				"app.kubernetes.io/component":  component,
				"gastown.io/rig":               rigName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "gastown.io/v1",
					Kind:       "Rig",
					Name:       rig.GetName(),
					UID:        rig.GetUID(),
				},
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/component": component,
					"gastown.io/rig":              rigName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "gt-operator",
						"app.kubernetes.io/component":  component,
						"gastown.io/rig":               rigName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  component,
							Image: r.agentImage,
							Env: []corev1.EnvVar{
								{Name: "GT_RUNTIME", Value: "k8s"},
								{Name: "GT_RIG", Value: rigName},
								{Name: "GT_ROLE", Value: role},
								{Name: "GT_NAMESPACE", Value: r.namespace},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "rig-storage",
									MountPath: "/gt/" + rigName,
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
					},
				},
			},
		},
	}

	log.Printf("[rig] creating %s deployment for rig %s", component, rigName)
	_, err = r.kubeClient.AppsV1().Deployments(r.namespace).Create(ctx, deploy, metav1.CreateOptions{})
	return err
}

// updateStatus updates the status subresource of a Rig CRD.
func (r *RigReconciler) updateStatus(ctx context.Context, rig *unstructured.Unstructured, phase, message string) {
	status := map[string]interface{}{
		"phase":         phase,
		"witnessReady":  phase == "Ready",
		"refineryReady": phase == "Ready",
	}
	if message != "" {
		status["message"] = message
	}

	patch := rig.DeepCopy()
	patch.Object["status"] = status
	_, err := r.dynamicClient.Resource(rigGVR).Namespace(r.namespace).UpdateStatus(ctx, patch, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("[rig] failed to update status for %s: %v", rig.GetName(), err)
	}
}
