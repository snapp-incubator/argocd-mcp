# argocd-mcp

A **read-only** [Model Context Protocol](https://modelcontextprotocol.io) server
that exposes ArgoCD **Applications** and **AppProjects** to an AI agent —
sync/health, sources, per-resource status, and per-user access.

It is built for a **multi-tenant** ArgoCD (many teams, one `user-argocd`
namespace) driven by the SnappCloud bot. It offers two families of tools:

- **Namespace-scoped** tools take a `namespace` argument (the team's own /
  destination namespace) and return objects keyed by that namespace, so the
  bot's existing namespace authorization scopes them per team.
- **Identity-scoped** tools answer "what can *I* access" from the caller's
  identity — they resolve the caller's OpenShift **groups** and match them
  against **AppProject roles** (exactly how ArgoCD authorizes), returning each
  accessible project with the caller's capability (admin / sync / view).

`argocd-mcp` holds **no ArgoCD credentials** and never calls the ArgoCD API — it
reads the `argoproj.io` CRs from the Kubernetes API with a read-only
ServiceAccount.

## Tools

Namespace-scoped (arg `namespace` = the team / destination namespace, never
`user-argocd`):

| Tool | Purpose |
|---|---|
| `argocd_list_applications` | Applications deploying into a namespace: project, sync, health, source. |
| `argocd_get_application` | One Application in depth: conditions, last sync operation, per-resource sync/health. |
| `argocd_get_appproject` | The AppProject governing a namespace: source repos, destinations, roles. |

Identity-scoped (no namespace arg; scoped to the **caller**, see *Identity* below):

| Tool | Purpose |
|---|---|
| `argocd_my_projects` | Every project the caller can access, each with capability (admin/sync/view) + namespaces. |
| `argocd_my_applications` | Every Application across the caller's projects, with sync/health + capability. |
| `argocd_my_namespaces` | Destination namespaces the caller's projects deploy into. |
| `argocd_can_i` | Whether the caller may perform an action (`sync`, `get`, `delete`, `action/apps/Deployment/restart`, …) on an application. |

All tools are strictly read-only (`get`/`list` only).

## Identity & authorization

Identity-scoped tools read the caller from the **`X-Remote-User`** request
header, set by the trusted upstream (the bot) from the authenticated user —
**never** from a tool argument, so a model cannot spoof it. A request without it
is refused by those tools.

Given the caller, the server:

1. resolves the caller's **groups** (`groups.Resolver`), and
2. matches them against every AppProject's roles + the global `argocd-rbac-cm`
   grants to compute the caller's **access** (`access.Resolver`).

Both steps are **behind interfaces so they can be swapped** without touching the
tools:

- [`internal/groups`](internal/groups) — `Resolver.GroupsFor(user) -> []group`.
  Default impl reads OpenShift `user.openshift.io` Group objects (the same source
  ArgoCD's Dex OpenShift connector reads at login; live, no re-login lag). Swap
  this to back group membership with an IAM/directory service.
- [`internal/access`](internal/access) — `Resolver.AccessFor(user) -> UserAccess`.
  Default impl composes the group resolver with AppProject/rbac-cm reads. This is
  the broader swap boundary: an IAM/RBAC backend can compute access its own way.

Capability is derived from the role's **policies** (not role-name convention), so
custom roles classify correctly; `argocd_can_i` evaluates the actual policy
globs.

## Running

```bash
argocd-mcp -mcp                 # stdio (local MCP clients)
argocd-mcp -http-addr=:8080     # streamable HTTP (default; POST /mcp)
```

In-cluster it uses the pod ServiceAccount; locally it falls back to your
kubeconfig. Endpoints: `/mcp`, `/healthz`, `/readyz`, `/version`.

### Configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `ARGOCD_NAMESPACE` | `user-argocd` | Namespace holding AppProjects + the RBAC ConfigMap. |
| `ARGOCD_RBAC_CONFIGMAP` | `argocd-rbac-cm` | ConfigMap with the global admin/readonly grants. |
| `GROUP_CACHE_TTL` | `1m` | How long the user→groups map is cached. |
| `K8S_TIMEOUT` / `K8S_QPS` / `K8S_BURST` | `30s` / `50` / `100` | Kubernetes client tuning. |
| `READINESS_TIMEOUT` | `10s` | Bounds the `/readyz` API-reachability probe. |

## RBAC

Read-only. Cluster-scoped (Applications/AppProjects can span namespaces;
`groups` are cluster-scoped):

```yaml
- apiGroups: ["argoproj.io"]
  resources: ["applications", "appprojects"]
  verbs: ["get", "list"]
- apiGroups: ["user.openshift.io"]
  resources: ["groups"]
  verbs: ["get", "list"]
```

Plus a namespaced Role (in `ARGOCD_NAMESPACE`) granting `get` on **only** the
RBAC ConfigMap (`resourceNames: [argocd-rbac-cm]`).

Deployed per cluster via the `argocd-mcp` Helm chart in the platform infra repo,
behind the private Contour ingress with basic auth; the bot is the only client.
