package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/snapp-incubator/argocd-mcp/internal/k8s"
)

// ArgoCD custom resources. Applications and AppProjects are read cluster-wide
// (the ServiceAccount holds a read-only ClusterRole) because Applications may
// live in more than one namespace under apps-in-any-namespace; each object is
// then scoped by its DESTINATION namespace, not by where the CR lives.
var (
	appGVR = schema.GroupVersionResource{
		Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
	}
	projGVR = schema.GroupVersionResource{
		Group: "argoproj.io", Version: "v1alpha1", Resource: "appprojects",
	}
)

// argocdNamespace is only used to surface where the CRs live in errors; reads
// are cluster-wide. Overridable for non-default installs.
func argocdNamespace() string {
	if v := os.Getenv("ARGOCD_NAMESPACE"); v != "" {
		return v
	}
	return "user-argocd"
}

// --- tool handlers ---

func handleListApplications(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	if ns == "" {
		return nil, fmt.Errorf("namespace is required (your team/destination namespace)")
	}
	project := a.str("project")

	list, err := c.Dynamic.Resource(appGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}

	out := make([]map[string]any, 0)
	for i := range list.Items {
		item := &list.Items[i]
		if destinationNamespace(item) != ns {
			continue
		}
		if project != "" && appProject(item) != project {
			continue
		}
		out = append(out, applicationSummary(item))
	}
	sort.Slice(out, func(i, j int) bool { return name(out[i]) < name(out[j]) })
	return map[string]any{"namespace": ns, "count": len(out), "applications": out}, nil
}

func handleGetApplication(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns, appName := a.str("namespace"), a.str("name")
	if ns == "" || appName == "" {
		return nil, fmt.Errorf("both namespace and name are required")
	}

	list, err := c.Dynamic.Resource(appGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		// Match by name AND destination namespace: the caller is authorized for
		// `ns`, so we only ever return an app that actually targets it — a name
		// alone can never reach into another tenant's namespace.
		if item.GetName() == appName && destinationNamespace(item) == ns {
			return applicationDetail(item), nil
		}
	}
	return nil, fmt.Errorf("application %q deploying into namespace %q not found in %s",
		appName, ns, argocdNamespace())
}

func handleGetAppProject(ctx context.Context, c *k8s.Client, a args) (any, error) {
	ns := a.str("namespace")
	if ns == "" {
		return nil, fmt.Errorf("namespace is required (your team/destination namespace)")
	}

	list, err := c.Dynamic.Resource(projGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list appprojects: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == ns || projectTargetsNamespace(item, ns) {
			return appProjectDetail(item, ns), nil
		}
	}
	return nil, fmt.Errorf("no AppProject governs namespace %q", ns)
}

// --- summaries (keyed by destination namespace, NEVER the argocd namespace) ---

func applicationSummary(u *unstructured.Unstructured) map[string]any {
	repo, path, targetRev := primarySource(u)
	m := map[string]any{
		"name": u.GetName(),
		// Scoping key: the destination namespace, so the caller's namespace
		// filter gates this record by the tenant it belongs to.
		"namespace":       destinationNamespace(u),
		"project":         appProject(u),
		"sync":            nestedString(u.Object, "status", "sync", "status"),
		"health":          nestedString(u.Object, "status", "health", "status"),
		"repo":            repo,
		"path":            path,
		"target_revision": targetRev,
	}
	if auto := hasAutomatedSync(u); auto {
		m["auto_sync"] = true
	}
	return pruneEmpty(m)
}

func applicationDetail(u *unstructured.Unstructured) map[string]any {
	m := applicationSummary(u)

	if msg := nestedString(u.Object, "status", "health", "message"); msg != "" {
		m["health_message"] = msg
	}

	// Conditions (e.g. ComparisonError, SyncError) — the usual smoking gun.
	if conds, ok := nestedSlice(u.Object, "status", "conditions"); ok {
		cs := make([]map[string]any, 0, len(conds))
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			cs = append(cs, pruneEmpty(map[string]any{
				"type":    asString(cm["type"]),
				"message": asString(cm["message"]),
			}))
		}
		if len(cs) > 0 {
			m["conditions"] = cs
		}
	}

	// Last sync operation outcome.
	if op, ok := nestedMap(u.Object, "status", "operationState"); ok {
		last := pruneEmpty(map[string]any{
			"phase":       asString(op["phase"]),
			"message":     asString(op["message"]),
			"finished_at": asString(op["finishedAt"]),
			"revision":    nestedString(op, "syncResult", "revision"),
		})
		if len(last) > 0 {
			m["last_operation"] = last
		}
	}

	// Per-resource sync/health. Each record carries its own namespace (the
	// destination namespace for namespaced resources, empty for cluster-scoped),
	// which the caller's filter also gates.
	if res, ok := nestedSlice(u.Object, "status", "resources"); ok {
		rs := make([]map[string]any, 0, len(res))
		for _, r := range res {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			rs = append(rs, pruneEmpty(map[string]any{
				"group":     asString(rm["group"]),
				"kind":      asString(rm["kind"]),
				"name":      asString(rm["name"]),
				"namespace": asString(rm["namespace"]),
				"sync":      asString(rm["status"]),
				"health":    nestedString(rm, "health", "status"),
			}))
		}
		if len(rs) > 0 {
			m["resources"] = rs
		}
	}
	return m
}

func appProjectDetail(u *unstructured.Unstructured, ns string) map[string]any {
	m := map[string]any{
		"name":      u.GetName(),
		"namespace": ns, // scoping key = the tenant namespace asked about
	}
	if repos, ok := nestedStringSlice(u.Object, "spec", "sourceRepos"); ok {
		m["source_repos"] = repos
	}
	if dests, ok := nestedSlice(u.Object, "spec", "destinations"); ok {
		ds := make([]map[string]any, 0, len(dests))
		for _, d := range dests {
			dm, ok := d.(map[string]any)
			if !ok {
				continue
			}
			ds = append(ds, pruneEmpty(map[string]any{
				"namespace": asString(dm["namespace"]),
				"server":    asString(dm["server"]),
			}))
		}
		if len(ds) > 0 {
			m["destinations"] = ds
		}
	}
	if roles, ok := nestedSlice(u.Object, "spec", "roles"); ok {
		rs := make([]map[string]any, 0, len(roles))
		for _, r := range roles {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			role := pruneEmpty(map[string]any{"name": asString(rm["name"])})
			if groups := toStringSlice(rm["groups"]); len(groups) > 0 {
				role["groups"] = groups
			}
			if policies := toStringSlice(rm["policies"]); len(policies) > 0 {
				role["policies"] = policies
			}
			rs = append(rs, role)
		}
		if len(rs) > 0 {
			m["roles"] = rs
		}
	}
	return m
}

// --- field accessors (pure; unit-tested) ---

func destinationNamespace(u *unstructured.Unstructured) string {
	return nestedString(u.Object, "spec", "destination", "namespace")
}

func appProject(u *unstructured.Unstructured) string {
	return nestedString(u.Object, "spec", "project")
}

// primarySource returns repoURL/path/targetRevision from spec.source, falling
// back to the first entry of spec.sources (multi-source Applications).
func primarySource(u *unstructured.Unstructured) (repo, path, targetRev string) {
	if src, ok := nestedMap(u.Object, "spec", "source"); ok {
		return asString(src["repoURL"]), asString(src["path"]), asString(src["targetRevision"])
	}
	if srcs, ok := nestedSlice(u.Object, "spec", "sources"); ok && len(srcs) > 0 {
		if src, ok := srcs[0].(map[string]any); ok {
			return asString(src["repoURL"]), asString(src["path"]), asString(src["targetRevision"])
		}
	}
	return "", "", ""
}

func hasAutomatedSync(u *unstructured.Unstructured) bool {
	_, ok := nestedMap(u.Object, "spec", "syncPolicy", "automated")
	return ok
}

func projectTargetsNamespace(u *unstructured.Unstructured, ns string) bool {
	dests, ok := nestedSlice(u.Object, "spec", "destinations")
	if !ok {
		return false
	}
	for _, d := range dests {
		if dm, ok := d.(map[string]any); ok {
			if asString(dm["namespace"]) == ns {
				return true
			}
		}
	}
	return false
}

// --- small generic helpers over unstructured maps ---

func nestedString(obj map[string]any, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj, fields...)
	return s
}

func nestedMap(obj map[string]any, fields ...string) (map[string]any, bool) {
	m, ok, _ := unstructured.NestedMap(obj, fields...)
	return m, ok
}

func nestedSlice(obj map[string]any, fields ...string) ([]any, bool) {
	s, ok, _ := unstructured.NestedSlice(obj, fields...)
	return s, ok
}

func nestedStringSlice(obj map[string]any, fields ...string) ([]string, bool) {
	s, ok, _ := unstructured.NestedStringSlice(obj, fields...)
	return s, ok
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func name(m map[string]any) string { return asString(m["name"]) }

// pruneEmpty drops keys whose value is an empty string, so summaries stay terse.
func pruneEmpty(m map[string]any) map[string]any {
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
	return m
}
