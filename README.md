# itz-deployer-operator

A production Golang Kubernetes Operator built for IBM TechZone that automates large-scale provisioning of complex IBM software on Red Hat OpenShift. Deployed internally at IBM to support hundreds of concurrent users across multi-tenant clusters.

## Overview

The operator watches for `Deployment` custom resources and fully automates the end-to-end provisioning lifecycle — from cloning a Git repository and bootstrapping ArgoCD, to executing Tekton pipelines or Ansible jobs, surfacing real-time status on the OpenShift Console, and triggering post-deployment workflows automatically.

A single `Deployment` CR replaces what previously required manual intervention across multiple tools, reducing deployment time by ~70% and enabling self-service provisioning for hundreds of users.

## How It Works

```
User applies Deployment CR
        │
        ▼
Controller reconciles desired state
        │
        ├─ Fetches IBM API Key from cluster secret
        ├─ Retrieves GitHub PAT from IBM Secrets Manager
        ├─ Clones target Git repository at specified release tag
        │
        ├─ Ansible playbook specified?
        │       └─ Creates Kubernetes Job running Ansible
        │
        └─ Default path:
                └─ Reads pipeline.yaml + pipelinerun.yaml from repo
                └─ Creates Tekton Pipeline + PipelineRun
        │
        ▼
Status reconciliation loop (every 30s)
        ├─ Watches PipelineRun or Job conditions
        ├─ Updates Deployment CR .status.phase
        └─ Updates OpenShift ConsoleNotification banner
        │
        ▼
Post-deployment (optional)
        └─ If spec.postDeployment is set and phase == Succeeded:
                └─ Creates a new child Deployment CR automatically
                └─ Owner reference set for cascading garbage collection
```

## Custom Resource

```yaml
apiVersion: techzone.techzone.ibm.com/v1alpha1
kind: Deployment
metadata:
  name: my-ibm-software
  namespace: default
spec:
  # Git repository name under github.ibm.com/itz-content
  repoName: my-software-repo

  # Git tag or branch to deploy
  release: v1.2.0

  # Optional parameters file passed to the pipeline
  parameters: "my-parameters-here.yaml"

  # Optional: run an Ansible playbook instead of Tekton
  ansible:
    ansiblePlaybook: playbooks/deploy.yaml
    requirements: requirements.yaml

  # Optional: trigger a second deployment as a post-deploy config after this one succeeds
  postDeployment:
    repoName: my-post-deploy-repo
    release: v1.0.0
    parameters: "my-post-deploy-parameters-here.yaml"
```

### Status

The operator continuously updates the CR status:

```yaml
status:
  phase: Running        # Pending | Running | Succeeded | Failed
  message: "Kubernetes Job is in progress."
  pipelineRunName: my-ibm-software-pipelinerun-xk9p2
  conditions:
    - type: Ready
      status: "True"
```

## Architecture

### Controller (`internal/controller`)

The reconcile loop handles the full deployment lifecycle:

- **Phase gating** — only progresses through phases in order, never re-runs a completed deployment
- **Dual execution engine** — routes to Tekton or Ansible based on spec, using a clean switch on `spec.ansible.ansiblePlaybook`
- **Status normalization** — maps both `tektonv1.PipelineRun` and `batchv1.Job` conditions to a common `phase` string using the Knative `apis.Condition` type
- **Post-deployment chaining** — creates child `Deployment` CRs with owner references, enabling multi-stage provisioning workflows
- **Console notifications** — reconciles `ConsoleNotification` resources on OpenShift to surface deployment status as a banner in the web console, with color-coded severity (red for failed, blue for running, green for succeeded)
- **MaxConcurrentReconciles: 5** — supports parallel reconciliation across multiple deployments

### Packages (`pkg`)

| Package | Responsibility |
|---|---|
| `pkg/argocd` | Bootstraps an ArgoCD instance on the cluster — creates the `ArgoCD` CR with production-tuned resource limits, waits for readiness, then deploys a pre-configured `Application` pointing at the Tekton tasks repository |
| `pkg/ibm` | IBM Secrets Manager integration — retrieves IBM API keys from cluster secrets and fetches GitHub PATs from IBM Secrets Manager v2 using IAM authentication |
| `pkg/rbac` | Bootstraps pipeline RBAC — creates `ClusterRoleBinding` for the Tekton pipeline service account, retrieves GitHub credentials via IBM Secrets Manager, and mounts them as secrets to the service account |
| `pkg/utils` | Kubernetes client factory — registers all required schemes including OpenShift, Tekton, External Secrets, and custom API types |

### Secrets Management

Credentials are never stored in manifests. The operator retrieves them dynamically at runtime:

1. IBM API key fetched from a Kubernetes secret in `kube-system`
2. IBM Secrets Manager client created using IAM authentication
3. GitHub PAT retrieved from Secrets Manager by secret ID
4. Kubernetes secret created and mounted to the pipeline service account

Exponential backoff with 360s max elapsed time wraps every secrets retrieval call, making the operator resilient to transient Secrets Manager failures.

## Technical Decisions

**Why unstructured client for ArgoCD resources?**
The ArgoCD operator CRDs (`argoproj.io/v1beta1`) are not available as Go types in public modules. Rather than vendoring the entire ArgoCD operator, the `pkg/argocd` package uses `unstructured.Unstructured` with typed Go structs for the spec and `runtime.DefaultUnstructuredConverter` to bridge between them. This avoids a heavyweight dependency while retaining compile-time safety for the fields we control.

**Why owner references on post-deployment CRs?**
Setting `ctrl.SetControllerReference` on child `Deployment` CRs means Kubernetes automatically garbage collects post-deployment resources when the parent is deleted. This prevents orphaned resources without requiring custom cleanup logic.

**Why ConsoleNotification reconciliation?**
OpenShift users provisioning IBM software often don't have direct access to `kubectl`. The ConsoleNotification banner surfaces deployment status in the OpenShift web console without requiring CLI access, improving self-service visibility for non-technical users.

## Stack

- **Language:** Go 1.24
- **Framework:** controller-runtime (Operator SDK scaffolding)
- **Execution engines:** Tekton Pipelines, Kubernetes Jobs (Ansible)
- **Secrets:** IBM Secrets Manager v2
- **GitOps:** ArgoCD (OpenShift GitOps)
- **Platform:** Red Hat OpenShift
- **CI:** GitHub Actions

## Development

### Prerequisites

- Go 1.24+
- Access to a Red Hat OpenShift cluster
- IBM Secrets Manager instance
- `kubectl` / `oc` CLI

### Running locally

```bash
# Install CRDs
make install

# Run the controller locally against your cluster
make run
```

### Building and deploying

```bash
# Build and push the operator image
make docker-build docker-push IMG=<registry>/itz-deployer-operator:tag

# Deploy to cluster
make deploy IMG=<registry>/itz-deployer-operator:tag
```

### Running tests

```bash
make test
```

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.