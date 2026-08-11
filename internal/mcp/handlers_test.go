package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// app builds an Application unstructured object as it appears in the API: the CR
// lives in the shared argocd namespace (metadata.namespace=user-argocd) but
// deploys into a tenant namespace (spec.destination.namespace).
func app(name, metaNS, destNS, project string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": metaNS},
		"spec": map[string]any{
			"project":     project,
			"destination": map[string]any{"namespace": destNS, "server": "https://kubernetes.default.svc"},
			"source":      map[string]any{"repoURL": "https://git/x.git", "path": "app", "targetRevision": "main"},
		},
		"status": map[string]any{
			"sync":   map[string]any{"status": "Synced"},
			"health": map[string]any{"status": "Healthy"},
		},
	}}
}

func TestApplicationSummary_ScopedByDestinationNamespace(t *testing.T) {
	got := applicationSummary(app("my-app", "user-argocd", "dba", "dba"))

	if got["namespace"] != "dba" {
		t.Fatalf("scoping namespace = %v, want dba (the destination namespace)", got["namespace"])
	}
	// The shared argocd namespace must never appear anywhere in the record, or
	// the bot's namespace filter would drop every app for every tenant.
	blob, _ := json.Marshal(got)
	if strings.Contains(string(blob), "user-argocd") {
		t.Fatalf("summary leaked the argocd namespace: %s", blob)
	}
	for _, k := range []string{"name", "project", "sync", "health", "repo"} {
		if _, ok := got[k]; !ok {
			t.Errorf("summary missing %q: %v", k, got)
		}
	}
}

func TestListFilter_OnlyRequestedDestinationNamespace(t *testing.T) {
	items := []*unstructured.Unstructured{
		app("dba-1", "user-argocd", "dba", "dba"),
		app("dba-2", "user-argocd", "dba", "dba"),
		app("routing-1", "user-argocd", "routing", "routing"),
	}
	// Mirror handleListApplications' filtering (destination namespace == ns).
	var kept []map[string]any
	for _, it := range items {
		if destinationNamespace(it) == "dba" {
			kept = append(kept, applicationSummary(it))
		}
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d apps, want 2 (dba only)", len(kept))
	}
	for _, r := range kept {
		if r["namespace"] != "dba" {
			t.Errorf("leaked non-dba app: %v", r)
		}
	}
}

func TestApplicationDetail_ResourcesAndConditions(t *testing.T) {
	u := app("my-app", "user-argocd", "dba", "dba")
	status := u.Object["status"].(map[string]any)
	status["sync"] = map[string]any{"status": "OutOfSync"}
	status["health"] = map[string]any{"status": "Degraded", "message": "pod crashloop"}
	status["conditions"] = []any{
		map[string]any{"type": "SyncError", "message": "boom"},
	}
	status["operationState"] = map[string]any{
		"phase":      "Failed",
		"message":    "sync failed",
		"finishedAt": "2026-08-11T00:00:00Z",
		"syncResult": map[string]any{"revision": "abc123"},
	}
	status["resources"] = []any{
		map[string]any{"kind": "Deployment", "name": "web", "namespace": "dba",
			"status": "OutOfSync", "health": map[string]any{"status": "Degraded"}},
	}

	got := applicationDetail(u)

	if got["sync"] != "OutOfSync" || got["health"] != "Degraded" {
		t.Fatalf("wrong sync/health: %v", got)
	}
	if got["health_message"] != "pod crashloop" {
		t.Errorf("missing health_message: %v", got["health_message"])
	}
	last, ok := got["last_operation"].(map[string]any)
	if !ok || last["phase"] != "Failed" || last["revision"] != "abc123" {
		t.Errorf("wrong last_operation: %v", got["last_operation"])
	}
	res, ok := got["resources"].([]map[string]any)
	if !ok || len(res) != 1 || res[0]["namespace"] != "dba" || res[0]["health"] != "Degraded" {
		t.Errorf("wrong resources: %v", got["resources"])
	}
	blob, _ := json.Marshal(got)
	if strings.Contains(string(blob), "user-argocd") {
		t.Fatalf("detail leaked the argocd namespace: %s", blob)
	}
}

func TestPrimarySource_MultiSourceFallback(t *testing.T) {
	u := app("m", "user-argocd", "dba", "dba")
	spec := u.Object["spec"].(map[string]any)
	delete(spec, "source")
	spec["sources"] = []any{
		map[string]any{"repoURL": "https://git/a.git", "path": "a", "targetRevision": "v1"},
		map[string]any{"repoURL": "https://git/b.git"},
	}
	repo, path, rev := primarySource(u)
	if repo != "https://git/a.git" || path != "a" || rev != "v1" {
		t.Fatalf("multi-source fallback wrong: %q %q %q", repo, path, rev)
	}
}

func proj(name, destNS string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "AppProject",
		"metadata":   map[string]any{"name": name, "namespace": "user-argocd"},
		"spec": map[string]any{
			"sourceRepos":  []any{"https://charts.example.com"},
			"destinations": []any{map[string]any{"namespace": destNS, "server": "*"}},
			"roles": []any{
				map[string]any{
					"name":     name + "-admin",
					"groups":   []any{name + "-admin", name + "-admin-ci"},
					"policies": []any{"p, proj:" + name + ":" + name + "-admin, applications, *, " + name + "/*, allow"},
				},
			},
		},
	}}
}

func TestAppProjectDetail_ScopedAndRolesSurfaced(t *testing.T) {
	got := appProjectDetail(proj("dba", "dba"), "dba")

	if got["namespace"] != "dba" {
		t.Fatalf("scoping namespace = %v, want dba", got["namespace"])
	}
	roles, ok := got["roles"].([]map[string]any)
	if !ok || len(roles) != 1 || roles[0]["name"] != "dba-admin" {
		t.Fatalf("roles not surfaced: %v", got["roles"])
	}
	groups, _ := roles[0]["groups"].([]string)
	if len(groups) != 2 || groups[0] != "dba-admin" {
		t.Errorf("groups wrong: %v", roles[0]["groups"])
	}
	if !projectTargetsNamespace(proj("dba", "dba"), "dba") {
		t.Error("projectTargetsNamespace should match its destination")
	}
	if projectTargetsNamespace(proj("dba", "dba"), "routing") {
		t.Error("projectTargetsNamespace matched a foreign namespace")
	}
}
