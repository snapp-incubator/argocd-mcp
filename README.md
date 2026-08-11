# argocd-mcp

A **read-only** [Model Context Protocol](https://modelcontextprotocol.io) server
that exposes a team's ArgoCD **Applications** and **AppProject** to an AI agent —
sync/health, sources, per-resource status, and project roles.

It is built for a **multi-tenant** ArgoCD (many teams, one `user-argocd`
namespace) driven by an agent that authorizes per **namespace** (the SnappCloud
bot + [`mcp-authz`](https://github.com/snapp-incubator/mcp-authz)).

## How tenant isolation works

`argocd-mcp` holds **no ArgoCD credentials** and does **not** call the ArgoCD
API. It reads the `argoproj.io` CRs from the Kubernetes API with a read-only
ServiceAccount, and every record it returns is keyed by the object's
**destination namespace** — `spec.destination.namespace` for an Application, the
project's destination for an AppProject — **never** the shared `user-argocd`
namespace the CRs live in.

That matters because the caller (the bot) already enforces namespace scope on
every tool result: it drops any record naming a namespace the user isn't
authorized for. Since an ArgoCD Application's destination namespace equals the
team's own namespace (and its AppProject name), the bot's existing namespace
authorization scopes ArgoCD objects per team automatically — no ArgoCD RBAC
reconstruction, no impersonation, no per-user tokens.

Every tool takes a required `namespace` argument = **the team / destination
namespace** (not `user-argocd`).

## Tools

| Tool | Purpose |
|---|---|
| `argocd_list_applications` | Applications deploying into a namespace: project, sync, health, source. |
| `argocd_get_application` | One Application in depth: conditions, last sync operation, per-resource sync/health. |
| `argocd_get_appproject` | The AppProject governing a namespace: source repos, destinations, roles (which groups hold which permissions). |

All tools are strictly read-only (`get`/`list` only).

## Running

```bash
# stdio (local MCP clients)
argocd-mcp -mcp

# streamable HTTP (default; endpoint POST /mcp on :8080)
argocd-mcp -http-addr=:8080
```

In-cluster it uses the pod ServiceAccount; locally it falls back to your
kubeconfig. Endpoints: `/mcp`, `/healthz`, `/readyz`, `/version`.

### Configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `ARGOCD_NAMESPACE` | `user-argocd` | Where the CRs live (used only in error messages; reads are cluster-wide). |
| `K8S_TIMEOUT` / `K8S_QPS` / `K8S_BURST` | `30s` / `50` / `100` | Kubernetes client tuning. |
| `READINESS_TIMEOUT` | `10s` | Bounds the `/readyz` API-reachability probe. |

## RBAC

The ServiceAccount needs only read on the ArgoCD CRs:

```yaml
- apiGroups: ["argoproj.io"]
  resources: ["applications", "appprojects"]
  verbs: ["get", "list"]
```

Deployed per cluster via the `argocd-mcp` Helm chart in the platform
infra repo, behind the private Contour ingress with basic auth; the bot is the
only client.
