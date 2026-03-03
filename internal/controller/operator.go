package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/anthropics/gt-operator/internal/shim"
)

var (
	polecatGVR = schema.GroupVersionResource{
		Group:    "gastown.io",
		Version:  "v1",
		Resource: "polecats",
	}
	rigGVR = schema.GroupVersionResource{
		Group:    "gastown.io",
		Version:  "v1",
		Resource: "rigs",
	}
)

// Config holds operator configuration.
type Config struct {
	Kubeconfig string
	Namespace  string
	AgentImage string
}

// Operator manages the Gas Town control plane in Kubernetes.
// It watches Polecat and Rig CRDs, reconciles desired state into pods,
// and runs the NudgeProcessor for cross-pod tmux operations.
type Operator struct {
	config        Config
	restConfig    *rest.Config
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface

	polecatReconciler *PolecatReconciler
	rigReconciler     *RigReconciler
	nudgeProcessor    *NudgeProcessor
}

// New creates a new Operator instance with initialized k8s clients.
func New(cfg Config) (*Operator, error) {
	restConfig, err := buildRestConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building k8s config: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	polecatReconciler := &PolecatReconciler{
		kubeClient:    kubeClient,
		dynamicClient: dynClient,
		namespace:     cfg.Namespace,
		agentImage:    cfg.AgentImage,
	}

	rigReconciler := &RigReconciler{
		kubeClient:    kubeClient,
		dynamicClient: dynClient,
		namespace:     cfg.Namespace,
		agentImage:    cfg.AgentImage,
	}

	nudgeProcessor := &NudgeProcessor{
		kubeClient:   kubeClient,
		restConfig:   restConfig,
		namespace:    cfg.Namespace,
		pollInterval: 2 * time.Second,
		router:       shim.NewMapRouter(),
	}

	return &Operator{
		config:            cfg,
		restConfig:        restConfig,
		kubeClient:        kubeClient,
		dynamicClient:     dynClient,
		polecatReconciler: polecatReconciler,
		rigReconciler:     rigReconciler,
		nudgeProcessor:    nudgeProcessor,
	}, nil
}

// Run starts the operator's reconciliation loops with leader election.
func (o *Operator) Run(ctx context.Context) error {
	log.Printf("[operator] starting in namespace %s (image: %s)", o.config.Namespace, o.config.AgentImage)

	// Use leader election to ensure only one operator instance is active.
	id, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("getting hostname: %w", err)
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "gt-operator-leader",
			Namespace: o.config.Namespace,
		},
		Client: o.kubeClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				if err := o.runLeader(ctx); err != nil {
					log.Printf("[operator] leader loop exited: %v", err)
				}
			},
			OnStoppedLeading: func() {
				log.Println("[operator] lost leadership, shutting down")
			},
			OnNewLeader: func(identity string) {
				if identity != id {
					log.Printf("[operator] new leader elected: %s", identity)
				}
			},
		},
	})

	return nil
}

// runLeader is called when this instance wins the leader election.
// It starts informers and reconciliation loops.
func (o *Operator) runLeader(ctx context.Context) error {
	log.Println("[operator] acquired leadership, starting informers")

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		o.dynamicClient,
		30*time.Second,
		o.config.Namespace,
		nil,
	)

	// Register Polecat informer with event handlers.
	polecatInformer := factory.ForResource(polecatGVR).Informer()
	polecatInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			log.Printf("[polecat] CRD added: %s", u.GetName())
			if err := o.polecatReconciler.Reconcile(ctx); err != nil {
				log.Printf("[polecat] reconcile error: %v", err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			u, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			log.Printf("[polecat] CRD updated: %s", u.GetName())
			if err := o.polecatReconciler.Reconcile(ctx); err != nil {
				log.Printf("[polecat] reconcile error: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				u, ok = tombstone.Obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
			}
			log.Printf("[polecat] CRD deleted: %s", u.GetName())
			if err := o.polecatReconciler.Reconcile(ctx); err != nil {
				log.Printf("[polecat] reconcile error: %v", err)
			}
		},
	})

	// Register Rig informer with event handlers.
	rigInformer := factory.ForResource(rigGVR).Informer()
	rigInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			log.Printf("[rig] CRD added: %s", u.GetName())
			if err := o.rigReconciler.Reconcile(ctx); err != nil {
				log.Printf("[rig] reconcile error: %v", err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			u, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			log.Printf("[rig] CRD updated: %s", u.GetName())
			if err := o.rigReconciler.Reconcile(ctx); err != nil {
				log.Printf("[rig] reconcile error: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				u, ok = tombstone.Obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
			}
			log.Printf("[rig] CRD deleted: %s", u.GetName())
			if err := o.rigReconciler.Reconcile(ctx); err != nil {
				log.Printf("[rig] reconcile error: %v", err)
			}
		},
	})

	// Start informers.
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	log.Println("[operator] informer caches synced")

	// Start nudge processor in a goroutine.
	go func() {
		log.Println("[nudge] starting processor")
		o.nudgeProcessor.Run(ctx)
	}()

	// Run initial reconciliation to sync any existing CRDs.
	if err := o.polecatReconciler.Reconcile(ctx); err != nil {
		log.Printf("[polecat] initial reconcile error: %v", err)
	}
	if err := o.rigReconciler.Reconcile(ctx); err != nil {
		log.Printf("[rig] initial reconcile error: %v", err)
	}

	// Block until context is cancelled.
	<-ctx.Done()
	log.Println("[operator] shutting down")
	return nil
}

// buildRestConfig creates a *rest.Config from kubeconfig path or in-cluster config.
func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
