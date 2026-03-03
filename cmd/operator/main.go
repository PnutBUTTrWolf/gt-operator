package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/anthropics/gt-operator/internal/controller"
)

var (
	Version   = "dev"
	Commit    = ""
	BuildTime = ""
)

func main() {
	var (
		kubeconfig string
		namespace  string
		gtImage    string
		showVer    bool
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (defaults to in-cluster)")
	flag.StringVar(&namespace, "namespace", "gastown", "Namespace to operate in")
	flag.StringVar(&gtImage, "agent-image", "gt-agent:latest", "Container image for agent pods")
	flag.BoolVar(&showVer, "version", false, "Print version and exit")
	flag.Parse()

	if showVer {
		fmt.Printf("gt-operator %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	op, err := controller.New(controller.Config{
		Kubeconfig: kubeconfig,
		Namespace:  namespace,
		AgentImage: gtImage,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create operator: %v\n", err)
		os.Exit(1)
	}

	if err := op.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "operator exited with error: %v\n", err)
		os.Exit(1)
	}
}
