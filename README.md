# argocd-mcp

A **strictly read-only** [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server that exposes ArgoCD **Applications** and **AppProjects** to an AI agent — sync/health, sources, per-resource status, projects, and per-caller access.

It is built for a **multi-tenant** ArgoCD (many teams, one shared `user-argocd` namespace) and is driven by the SnappCloud bot. It holds **no ArgoCD credentials** and never calls the ArgoCD API — it reads the `argoproj.io` custom resources straight from the Kubernetes API with a read-only ServiceAccount, and never mutates cluster state.

**Every tool is identity-scoped.** Each answers "what can *I* access?" from the caller's identity: it resolves the caller's OpenShift **groups** and matches them against **AppProject roles** — exactly how ArgoCD itself authorizes — returning each accessible project/application with the caller's **capability** (`admin` / `sync` / `view`). `namespace`, `project`, and `name` arguments are only optional **filters/selectors**, validated against that access — never the authorization mechanism — so the model can never reach another team's data.

## Contents

- [Tools](#tools)
- [How access is computed](#how-access-is-computed)
- [Identity & authorization](#identity--authorization)
- [Swappable by design](#swappable-by-design)
- [Tenant isolation & security](#tenant-isolation--security)
- [Running](#running)
- [Configuration (env)](#configuration-env)
- [RBAC](#rbac)
- [Project layout](#project-layout)
- [Development](#development)
- [Deployment](#deployment)

## Tools

All tools are strictly read-only (`get`/`list` only) and scoped to the **caller** (see [Identity & authorization](#identity--authorization)). Every tool needs a caller identity; a request without one is refused.

A `namespace` argument is always a **destination** namespace — the namespace an Application deploys into (`spec.destination.namespace`), which for a team equals its own namespace and usually its AppProject name. It is **never** the shared `user-argocd` namespace where the CRs live.

| Tool | Args | Purpose |
| --- | --- | --- |
| `argocd_list_applications` | `namespace?`, `project?` | The caller's Applications with project, sync status, health, source, and capability. Optional filters narrow to one destination namespace and/or project. |
| `argocd_get_application` | `name`, `namespace?` | One Application in depth: sync/health, source, conditions, last sync operation, and per-resource sync/health. `namespace` disambiguates apps that share a name across namespaces (apps-in-any-namespace). Returns a non-committal not-found if the caller has no access. |
| `argocd_list_projects` | `namespace?` | The caller's projects as summaries: capability, destination namespaces, and role names. `namespace` narrows to the projects that govern it. |
| `argocd_get_project` | `name?`, `namespace?` | Full project detail: allowed source repos, destinations, and roles. Select by `name` (one project) **or** `namespace` (**every** project whose destinations include it — a namespace may be governed by more than one). |
| `argocd_list_namespaces` | `project?` | The destination namespaces the caller's projects deploy into. Optional `project` narrows to one project's destinations. |
| `argocd_can_i` | `application`, `action` | Whether the caller may perform an action (`sync`, `get`, `delete`, `override`, `action/apps/Deployment/restart`, …) on an Application, with the reason, evaluated against the caller's ArgoCD role policies. |

**list vs get.** `list_*` return lightweight summaries for browsing; `get_*` return full detail for one object. Start with a `list_`, then drill into a specific app/project with the matching `get_`.

**Capability** is a coarse label derived from the caller's ArgoCD `applications` **policies** (not role-name convention), so custom roles classify correctly: `view` = read (get/logs); `sync` = view + sync/rollback/restart; `admin` = full control.

## How access is computed

```text
bot ──POST /mcp (X-Remote-User: alice@corp) ──▶ argocd-mcp
                                                  │ groups.Resolver: email → OpenShift groups
                                                  │ access.Resolver: groups × AppProject roles
                                                  │                  + global argocd-rbac-cm
                                                  └─▶ projects/apps the caller can access + capability
```

For each request the server:

1. reads the caller from the **`X-Remote-User`** header (see [Identity & authorization](#identity--authorization));
2. resolves the caller's **groups** (`groups.Resolver`) — by default from the OpenShift `user.openshift.io` Group objects;
3. matches those groups against every AppProject's `roles` and the global `argocd-rbac-cm` grants to compute the caller's **access** (`access.Resolver`): the projects they can reach, their destination namespaces, the effective policies, and the derived capability.

**Global grants** in `argocd-rbac-cm` are honored on top of per-project roles: a group mapped to `role:admin` → admin everywhere; `role:readonly` → view everywhere.

> **Why OpenShift Group objects (not Dex)?** The ArgoCD UI shows a user their groups, but Dex's gRPC API has no "groups by email" method (`GetUserIdentity` is a stale, login-only snapshot keyed by connector user id, and isn't exposed by ArgoCD's bundled Dex). The authoritative, **live** source is the OpenShift `Group` objects — the same source Dex's OpenShift connector reads at login — so that is what the default resolver reads.

## Identity & authorization

The caller identity is read from the **`X-Remote-User`** request header, set by the trusted upstream (the bot) from the authenticated user and lifted onto the request context by a small middleware ([`internal/mcp/identity.go`](internal/mcp/identity.go)). It is **never** taken from a tool argument, so a model cannot spoof it, and a request without it is refused.

Because identity is out-of-band from the model and every tool authorizes from it, the tools are marked **self-authorized** on the bot side: the bot forwards identity and trusts the server's scoped answer rather than re-filtering it.

## Swappable by design

The identity computation sits behind two interfaces, so the backing source can be replaced (e.g. an IAM/directory service) without touching the tools:

- [`internal/groups`](internal/groups) — `Resolver.GroupsFor(user) -> []group`. The narrow "group membership" seam. Default impl reads OpenShift `user.openshift.io` Group objects (cached; adds the implicit `system:authenticated*` groups). Swap this to back group membership with an IAM/directory service.
- [`internal/access`](internal/access) — `Resolver.AccessFor(user) -> UserAccess`. The broader swap boundary. Default impl composes the group resolver with AppProject + `argocd-rbac-cm` reads; an IAM/RBAC backend can compute `UserAccess` its own way.

## Tenant isolation & security

- **Read-only throughout.** Holds no ArgoCD credentials, never calls the ArgoCD API, and never mutates cluster state — all reads are `get`/`list`.
- **Identity can't be spoofed.** It comes only from the bot-set `X-Remote-User` header, out-of-band from the model.
- **Arguments can't widen access.** `namespace`/`project`/`name` are filters/selectors only — every handler first computes the caller's access, then filters within it — so a model choosing arguments can never reach another tenant's objects.
- **No cross-tenant enumeration.** `get_application` returns a non-committal not-found for an app the caller can't access (it does not distinguish "missing" from "inaccessible"); `list_*` / `get_project` simply omit projects outside the caller's access.
- **Records key on the destination namespace**, never the shared `user-argocd`, so nothing about the shared namespace leaks.

## Running

```bash
argocd-mcp -mcp                 # stdio (local MCP clients)
argocd-mcp -http-addr=:8080     # streamable HTTP (default; POST /mcp)
```

In-cluster it uses the pod ServiceAccount; locally it falls back to your kubeconfig. Over HTTP it is stateless (the tools are pure request/response, so no per-client session state accumulates).

Endpoints:

| Path | Purpose |
| --- | --- |
| `/mcp` | MCP over streamable HTTP (POST). Wrapped by the `X-Remote-User` identity middleware. |
| `/healthz` | Liveness — always 200 while the process is up. |
| `/readyz` | Readiness — a single `GET /version` against the Kubernetes API (no listing, no RBAC), bounded by `READINESS_TIMEOUT`. |
| `/version` | Build version as JSON. |

## Configuration (env)

| Var | Default | Meaning |
| --- | --- | --- |
| `ARGOCD_NAMESPACE` | `user-argocd` | Namespace holding the AppProjects + the RBAC ConfigMap. |
| `ARGOCD_RBAC_CONFIGMAP` | `argocd-rbac-cm` | ConfigMap with the global admin/readonly grants. |
| `GROUP_CACHE_TTL` | `1m` | How long the user→groups map is cached. |
| `READINESS_TIMEOUT` | `10s` | Bounds the `/readyz` API-reachability probe. |
| `K8S_KUBECONFIG` | *(in-cluster, else default kubeconfig)* | Explicit kubeconfig path (local runs). |
| `K8S_CONTEXT` | *(current context)* | Explicit kubeconfig context. |
| `K8S_TIMEOUT` / `K8S_QPS` / `K8S_BURST` | `30s` / `50` / `100` | Kubernetes client tuning. |

## RBAC

Read-only. Cluster-scoped (Applications/AppProjects can span namespaces under apps-in-any-namespace; `groups` are cluster-scoped):

```yaml
- apiGroups: ["argoproj.io"]
  resources: ["applications", "appprojects"]
  verbs: ["get", "list"]
- apiGroups: ["user.openshift.io"]
  resources: ["groups"]
  verbs: ["get", "list"]
```

Plus a namespaced Role (in `ARGOCD_NAMESPACE`) granting `get` on **only** the RBAC ConfigMap:

```yaml
- apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["argocd-rbac-cm"]
  verbs: ["get"]
```

## Project layout

```text
cmd/argocd-mcp/   entrypoint (stdio | http), health endpoints, identity middleware wiring
internal/mcp/     MCP server, tool definitions, identity-scoped handlers, X-Remote-User middleware
internal/access/  access resolver interface, UserAccess types, capability/policy logic, ArgoCD impl
internal/groups/  group-membership resolver interface + OpenShift Group-objects impl
internal/k8s/     read-only typed + dynamic Kubernetes clients
```

Built with the [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk); serves MCP over stdio and streamable HTTP. Ships as a distroless-nonroot image built to `ghcr.io`.

## Development

```bash
go build ./...
go vet ./...
go test ./...
golangci-lint run
```

Tests cover: group-object parsing and implicit-group merge (`internal/groups`); capability derivation from policies, `can_i` glob matching, and role→capability matching against a real AppProject including global admin/readonly overrides (`internal/access`); and the identity-scoped handlers (`internal/mcp`) — results confined to the caller's access, capability attached, inaccessible apps not-found without leaking existence, `namespace` disambiguation of same-named apps, `get_project` returning **every** project governing a namespace, and identity refusal without `X-Remote-User`. An end-to-end test drives the server over real HTTP to prove the `X-Remote-User` header reaches the handler through the SDK.

## Deployment

Deployed per cluster via the `argocd-mcp` Helm chart in the platform infra repo, behind the private Contour ingress with basic auth; [the SnappCloud Bot](https://github.com/snapp-incubator/snappcloud-bot/) is the only client, configured to forward identity (`sendIdentity`) and treat the tools as `selfAuthorized`.
