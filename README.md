# argocd-mcp

A **read-only** [Model Context Protocol](https://modelcontextprotocol.io) server
that exposes ArgoCD **Applications** and **AppProjects** to an AI agent —
sync/health, sources, per-resource status, and per-user access.

It is built for a **multi-tenant** ArgoCD (many teams, one `user-argocd`
namespace) driven by the SnappCloud bot. **Every tool is identity-scoped**: it
answers "what can *I* access" from the caller's identity — resolving the caller's
OpenShift **groups** and matching them against **AppProject roles** (exactly how
ArgoCD authorizes), returning each accessible project/application with the
caller's capability (admin / sync / view). `namespace`, `project`, and `name`
arguments are only optional **filters/selectors**, validated against that access
— never the authorization mechanism, so the model can never reach another team's
data.

`argocd-mcp` holds **no ArgoCD credentials** and never calls the ArgoCD API — it
reads the `argoproj.io` CRs from the Kubernetes API with a read-only
ServiceAccount.

## Tools

All tools are strictly read-only (`get`/`list` only) and scoped to the **caller**
(see *Identity & authorization* below). A `namespace` is a **destination**
namespace (the team's own namespace, `spec.destination.namespace`), never
`user-argocd`.

| Tool | Args | Purpose |
| --- | --- | --- |
| `argocd_list_applications` | `namespace?`, `project?` | The caller's Applications (project, sync, health, source, capability); optional filters. |
| `argocd_get_application` | `name`, `namespace?` | One Application in depth: conditions, last sync operation, per-resource sync/health. `namespace` disambiguates same-named apps. |
| `argocd_list_projects` | `namespace?` | The caller's projects as summaries: capability, namespaces, role names. `namespace` narrows to projects governing it. |
| `argocd_get_project` | `name?`, `namespace?` | Full project detail: source repos, destinations, roles. By `name` (one) **or** `namespace` (every project governing it — may be several). |
| `argocd_list_namespaces` | `project?` | Destination namespaces the caller's projects deploy into; optional `project` narrows. |
| `argocd_can_i` | `application`, `action` | Whether the caller may perform an action (`sync`, `get`, `delete`, `action/apps/Deployment/restart`, …) on an application. |

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
| --- | --- | --- |
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
