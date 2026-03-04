package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/anthropics/gt-operator/internal/shim"

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
	config    Config
	clientset kubernetes.Interface
	dynClient dynamic.Interface
	restCfg   *rest.Config
}

// New creates a new Operator instance.
func New(cfg Config) (*Operator, error) {
	restCfg, err := buildRestConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build k8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create k8s clientset: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}

	return &Operator{
		config:    cfg,
		clientset: clientset,
		dynClient: dynClient,
		restCfg:   restCfg,
	}, nil
}

// buildRestConfig loads kubeconfig from path or falls back to in-cluster config.
func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// Run starts the operator's reconciliation loops with leader election.
// It blocks until ctx is cancelled.
func (o *Operator) Run(ctx context.Context) error {
	log.Printf("[operator] starting in namespace %s (image: %s)", o.config.Namespace, o.config.AgentImage)

	hostname, err := getHostname()
	if err != nil {
		return fmt.Errorf("get hostname: %w", err)
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "gt-operator-leader",
			Namespace: o.config.Namespace,
		},
		Client: o.clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: hostname,
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
				if identity != hostname {
					log.Printf("[operator] new leader elected: %s", identity)
				}
			},
		},
	})

	return nil
}

// runLeader runs the reconciliation loops when this instance is the leader.
func (o *Operator) runLeader(ctx context.Context) error {
	log.Println("[operator] acquired leadership, starting controllers")

	// Session registry for cross-pod tmux routing
	sessionRouter := shim.NewMapRouter()

	// Initialize reconcilers
	polecatReconciler := &PolecatReconciler{
		clientset:     o.clientset,
		dynClient:     o.dynClient,
		namespace:     o.config.Namespace,
		agentImage:    o.config.AgentImage,
		sessionRouter: sessionRouter,
	}

	rigReconciler := &RigReconciler{
		clientset:  o.clientset,
		dynClient:  o.dynClient,
		namespace:  o.config.Namespace,
		agentImage: o.config.AgentImage,
	}

	// Set up dynamic informer factory for CRDs
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		o.dynClient, 30*time.Second, o.config.Namespace, nil,
	)

	// Register Polecat informer
	polecatInformer := factory.ForResource(polecatGVR).Informer()
	polecatInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			log.Printf("[operator] polecat added: %s", u.GetName())
			polecatReconciler.Reconcile(ctx, u)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			u, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			polecatReconciler.Reconcile(ctx, u)
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
			log.Printf("[operator] polecat deleted: %s", u.GetName())
			polecatReconciler.HandleDelete(ctx, u)
		},
	})

	// Register Rig informer
	rigInformer := factory.ForResource(rigGVR).Informer()
	rigInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			log.Printf("[operator] rig added: %s", u.GetName())
			rigReconciler.Reconcile(ctx, u)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			u, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			rigReconciler.Reconcile(ctx, u)
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
			log.Printf("[operator] rig deleted: %s", u.GetName())
			rigReconciler.HandleDelete(ctx, u)
		},
	})

	// Start informers
	var wg sync.WaitGroup
	factory.Start(ctx.Done())

	// Wait for caches to sync
	log.Println("[operator] waiting for informer caches to sync")
	factory.WaitForCacheSync(ctx.Done())
	log.Println("[operator] informer caches synced")

	// Collect rig mount points for NudgeProcessor from existing Rig resources
	rigMounts := o.collectRigMounts(ctx)

	// Start NudgeProcessor
	nudgeProcessor := NewNudgeProcessor(NudgeProcessorConfig{
		Router:    sessionRouter,
		Namespace: o.config.Namespace,
		RigMounts: rigMounts,
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := nudgeProcessor.Run(ctx); err != nil {
			log.Printf("[operator] nudge processor exited: %v", err)
		}
	}()

	log.Println("[operator] all controllers started")
	<-ctx.Done()
	log.Println("[operator] shutting down controllers")
	wg.Wait()
	return nil
}

// collectRigMounts lists existing Rig resources and returns their PVC mount paths.
func (o *Operator) collectRigMounts(ctx context.Context) []string {
	rigs, err := o.dynClient.Resource(rigGVR).Namespace(o.config.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[operator] failed to list rigs for nudge mounts: %v", err)
		return nil
	}

	var mounts []string
	for _, rig := range rigs.Items {
		name, _, _ := unstructured.NestedString(rig.Object, "spec", "name")
		if name == "" {
			name = rig.GetName()
		}
		mounts = append(mounts, fmt.Sprintf("/gt/%s", name))
	}

	log.Printf("[operator] collected %d rig mount(s) for nudge processor", len(mounts))
	return mounts
}

// getHostname returns the pod hostname for leader election identity.
func getHostname() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("os.Hostname: %w", err)
	}
	return hostname, nil
}
