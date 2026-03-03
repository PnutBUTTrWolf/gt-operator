# gt-operator

Kubernetes operator for [Gas Town](https://github.com/steveyegge/gastown) multi-agent workspaces. Runs Gas Town agents as containers in a Kubernetes cluster with the same tmux-based runtime they use locally.

## Architecture

The operator wraps the Gas Town daemon binary rather than replacing it. A tmux shim intercepts tmux calls and routes them across pods via `kubectl exec`, so upstream gastown updates flow through without modification.

```
┌──────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                   │
│                                                       │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  │
│  │ Polecat Pod  │  │ Polecat Pod  │  │ Polecat Pod  │  │
│  │ (tmux+agent) │  │ (tmux+agent) │  │ (tmux+agent) │  │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘  │
│         │                 │                 │          │
│         └────────┬────────┴────────┬────────┘          │
│                  │                 │                    │
│  ┌───────────────▼──┐  ┌──────────▼──────────────┐    │
│  │ Dolt StatefulSet  │  │ Operator Pod             │    │
│  │ :3307 (ClusterIP) │  │ ┌──────────────────────┐ │    │
│  │ PVC: dolt-data    │  │ │ gt daemon + tmux shim│ │    │
│  └───────────────────┘  │ │ (kubectl exec router)│ │    │
│                          │ └──────────────────────┘ │    │
│  ┌────────────┐          └──────────────────────────┘    │
│  │ Mayor Pod  │  ┌──────────┐  ┌──────────────┐        │
│  │            │  │ Witness  │  │ Refinery Pod │        │
│  └────────────┘  │ Pod      │  │              │        │
│                  └──────────┘  └──────────────┘        │
│                                                        │
│  ┌────────────────────────────────────────────────┐    │
│  │ RWX PVC (per rig)                               │    │
│  │ .repo.git/ | polecats/ | locks/ | refinery/    │    │
│  └────────────────────────────────────────────────┘    │
└────────────────────────────────────────────────────────┘
```

## Key Design Decisions

| Component | Approach |
|-----------|----------|
| Dolt database | StatefulSet + ClusterIP Service |
| Git repo sharing | RWX PVC per rig (NFS/EFS/Filestore) |
| Filesystem locks | Flock on the RWX PVC |
| Agent runtime | tmux inside each pod (unchanged from local) |
| Daemon | Runs inside operator pod with kubectl exec shim |
| Inter-agent comms | Queue mail via Dolt (unchanged); nudges via filesystem queue on PVC |
| Citadel integration | Local VS Code extension, port-forward for Dolt, kubectl exec for terminals |
| Container image | Single universal image for all agent roles |
| Polecats | Created/deleted via Polecat CRD lifecycle |
| Infrastructure agents | Deployments (mayor, deacon = town-level; witness, refinery = per-rig) |

## Components

### Operator (`cmd/operator/`)

Watches Polecat and Rig CRDs. Reconciles desired state into Pods, Deployments, and PVCs. Runs the gt daemon binary internally with the tmux shim for cross-pod operations.

### tmux Shim (`internal/shim/`)

Drop-in replacement for `/usr/bin/tmux`. Installed in all Gas Town containers at `/usr/local/bin/tmux`.

**Operator mode:** Routes remote tmux commands via `kubectl exec` using a session-to-pod registry.

**Agent mode:** Routes remote tmux commands via a nudge queue on the shared PVC. The operator polls the queue and executes on behalf of the agent. Agents never need kubectl access.

### CRDs (`deploy/crds/`)

- **Polecat** — Represents a polecat agent. Created by `gt sling`, maps to a Pod.
- **Rig** — Represents a rig. Creates RWX PVC, Witness Deployment, and Refinery Deployment.

### Helm Chart (`deploy/helm/gt-operator/`)

Deploys the full stack: namespace, RBAC, Dolt StatefulSet, operator Deployment, mayor Deployment, deacon Deployment.

### Container Images (`image/`)

- **Dockerfile** — Operator image (operator binary + tmux shim + kubectl)
- **Dockerfile.agent** — Universal agent image (tmux + git + gt + bd + claude CLI)

## Prerequisites

- Kubernetes 1.28+
- Helm 3
- A storage class that supports ReadWriteMany (e.g., EFS on AWS, Filestore on GCP, Azure Files)
- Container registry access for pushing images
- Claude Code CLI credentials (stored as a Kubernetes Secret)

## Quick Start

```bash
# Build
make build-all

# Build container images
make image
make image-agent

# Install CRDs
make install-crds

# Deploy via Helm
helm upgrade --install gt-operator deploy/helm/gt-operator \
  --set agent.image=your-registry/gt-agent:latest \
  --set storage.rwxStorageClass=efs-sc

# Add a rig
kubectl apply -f - <<EOF
apiVersion: gastown.io/v1
kind: Rig
metadata:
  name: myproject
  namespace: gastown
spec:
  name: myproject
  gitUrl: https://github.com/you/repo.git
  defaultBranch: main
  storageClass: efs-sc
EOF

# Sling work (creates a Polecat CRD → operator creates Pod)
gt sling my-bead myproject
```

## Rollout Strategy

1. **Stage 1** — Dolt in k8s, everything else local. Port-forward Dolt.
2. **Stage 2** — Single polecat in k8s. Validate full agent lifecycle.
3. **Stage 3** — Full migration. All agents in k8s. Citadel via port-forward + kubectl exec.

## Development

```bash
make build        # Build operator binary
make build-shim   # Build tmux shim binary
make build-all    # Build everything
make test         # Run tests
make clean        # Remove build artifacts
```

## License

[MIT](LICENSE)
