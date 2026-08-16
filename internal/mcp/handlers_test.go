package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/snapp-incubator/argocd-mcp/internal/access"
	"github.com/snapp-incubator/argocd-mcp/internal/k8s"
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

func TestProjectDetail_NameAndRolesSurfaced(t *testing.T) {
	got := projectDetail(proj("dba", "dba"))

	if got["project"] != "dba" {
		t.Fatalf("project = %v, want dba", got["project"])
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

// stubResolver returns a fixed UserAccess (with the caller's name filled in) so
// handler tests can exercise identity-scoped authorization without OpenShift.
type stubResolver struct{ ua access.UserAccess }

func (s stubResolver) AccessFor(_ context.Context, user string) (access.UserAccess, error) {
	out := s.ua
	out.User = user
	return out, nil
}

// ctxUser builds a context carrying the caller identity, as WithIdentity does.
func ctxUser(user string) context.Context {
	return context.WithValue(context.Background(), userKey{}, user)
}

// fakeProjects builds a dynamic client serving the given AppProjects.
func fakeProjects(objs ...runtime.Object) *k8s.Client {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{projGVR: "AppProjectList"}, objs...)
	return &k8s.Client{Dynamic: dyn}
}

// fakeApps builds a dynamic client serving the given Applications.
func fakeApps(objs ...runtime.Object) *k8s.Client {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{appGVR: "ApplicationList"}, objs...)
	return &k8s.Client{Dynamic: dyn}
}

// TestGetProject_ReturnsEveryProjectGoverningNamespace guards the original bug: a
// namespace that is a destination of more than one AppProject must surface all of
// them (the caller can access both), not just the first iterated.
func TestGetProject_ReturnsEveryProjectGoverningNamespace(t *testing.T) {
	c := fakeProjects(proj("team-a", "shared-ns"), proj("team-b", "shared-ns"), proj("team-c", "other-ns"))
	stub := stubResolver{ua: access.UserAccess{Projects: []access.ProjectAccess{
		{Project: "team-a", Namespaces: []string{"shared-ns"}, Capability: access.CapAdmin},
		{Project: "team-b", Namespaces: []string{"shared-ns"}, Capability: access.CapSync},
	}}}

	got, err := handleGetProject(stub)(ctxUser("alice@example.com"), c, args{"namespace": "shared-ns"})
	if err != nil {
		t.Fatalf("handleGetProject: %v", err)
	}
	projects := got.(map[string]any)["projects"].([]map[string]any)
	if len(projects) != 2 {
		t.Fatalf("got %d projects for shared-ns, want 2 (team-a and team-b)", len(projects))
	}
	names := map[string]bool{}
	for _, p := range projects {
		names[p["project"].(string)] = true
	}
	if !names["team-a"] || !names["team-b"] {
		t.Errorf("projects = %v, want both team-a and team-b", names)
	}
}

// TestGetProject_ExcludesInaccessible: team-b also governs shared-ns, but a caller
// without access to it must not see it (no enumeration of other tenants).
func TestGetProject_ExcludesInaccessible(t *testing.T) {
	c := fakeProjects(proj("team-a", "shared-ns"), proj("team-b", "shared-ns"))
	stub := stubResolver{ua: access.UserAccess{Projects: []access.ProjectAccess{
		{Project: "team-a", Namespaces: []string{"shared-ns"}, Capability: access.CapAdmin},
	}}}

	got, err := handleGetProject(stub)(ctxUser("alice@example.com"), c, args{"namespace": "shared-ns"})
	if err != nil {
		t.Fatalf("handleGetProject: %v", err)
	}
	projects := got.(map[string]any)["projects"].([]map[string]any)
	if len(projects) != 1 || projects[0]["project"] != "team-a" {
		t.Fatalf("want only team-a, got %v", projects)
	}
}

// TestGetProject_RequiresSelector: neither name nor namespace -> error.
func TestGetProject_RequiresSelector(t *testing.T) {
	if _, err := handleGetProject(stubResolver{})(ctxUser("alice"), fakeProjects(), args{}); err == nil {
		t.Error("expected error when neither name nor namespace given")
	}
}

// TestIdentityRequired: without X-Remote-User in context, handlers refuse.
func TestIdentityRequired(t *testing.T) {
	if _, err := handleGetProject(stubResolver{})(context.Background(), fakeProjects(), args{"namespace": "x"}); err == nil {
		t.Error("get_project: expected identity-required error without X-Remote-User")
	}
	if _, err := handleListApplications(stubResolver{})(context.Background(), fakeApps(), args{}); err == nil {
		t.Error("list_applications: expected identity-required error without X-Remote-User")
	}
}

// TestListApplications_ScopedToCallerAndFiltered: only apps in the caller's
// accessible projects are returned, capability is attached, and filters narrow.
func TestListApplications_ScopedToCallerAndFiltered(t *testing.T) {
	c := fakeApps(
		app("web", "user-argocd", "team-a-ns", "team-a"),
		app("api", "user-argocd", "team-a-ns", "team-a"),
		app("secret", "user-argocd", "team-b-ns", "team-b"), // caller has no access
	)
	stub := stubResolver{ua: access.UserAccess{Projects: []access.ProjectAccess{
		{Project: "team-a", Namespaces: []string{"team-a-ns"}, Capability: access.CapSync},
	}}}

	got, err := handleListApplications(stub)(ctxUser("alice"), c, args{})
	if err != nil {
		t.Fatal(err)
	}
	apps := got.(map[string]any)["applications"].([]map[string]any)
	if len(apps) != 2 {
		t.Fatalf("want 2 accessible apps, got %d: %v", len(apps), apps)
	}
	for _, a := range apps {
		if a["name"] == "secret" {
			t.Error("app in an inaccessible project leaked")
		}
		if a["capability"] != "sync" {
			t.Errorf("capability not attached: %v", a["capability"])
		}
	}
	// namespace filter that matches nothing accessible -> empty.
	got, _ = handleListApplications(stub)(ctxUser("alice"), c, args{"namespace": "team-b-ns"})
	if n := got.(map[string]any)["count"]; n != 0 {
		t.Errorf("namespace filter count = %v, want 0", n)
	}
}

// TestGetApplication_DenyAndDisambiguation: inaccessible apps are not-found (no
// leak); a name shared across namespaces needs the namespace disambiguator.
func TestGetApplication_DenyAndDisambiguation(t *testing.T) {
	// Same app name across different source (metadata) namespaces —
	// apps-in-any-namespace — so the destination namespace disambiguates.
	c := fakeApps(
		app("web", "team-a", "team-a-ns", "team-a"),
		app("web", "team-b", "team-b-ns", "team-b"),
		app("secret", "team-c", "team-c-ns", "team-c"),
	)
	stub := stubResolver{ua: access.UserAccess{Projects: []access.ProjectAccess{
		{Project: "team-a", Namespaces: []string{"team-a-ns"}, Capability: access.CapAdmin},
		{Project: "team-b", Namespaces: []string{"team-b-ns"}, Capability: access.CapView},
	}}}

	// Inaccessible app -> found=false, no existence leak.
	got, err := handleGetApplication(stub)(ctxUser("alice"), c, args{"name": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["found"] != false {
		t.Errorf("expected found=false for inaccessible app, got %v", got)
	}

	// Ambiguous accessible name -> error asking to disambiguate.
	if _, err := handleGetApplication(stub)(ctxUser("alice"), c, args{"name": "web"}); err == nil {
		t.Error("expected ambiguity error for duplicate accessible name")
	}

	// Namespace disambiguates -> exactly that app, with capability.
	got, err = handleGetApplication(stub)(ctxUser("alice"), c, args{"name": "web", "namespace": "team-b-ns"})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["namespace"] != "team-b-ns" || m["capability"] != "view" {
		t.Errorf("disambiguation returned wrong app/capability: %v", m)
	}
}

// TestListNamespaces_ProjectFilter narrows to one project's destinations.
func TestListNamespaces_ProjectFilter(t *testing.T) {
	stub := stubResolver{ua: access.UserAccess{Projects: []access.ProjectAccess{
		{Project: "team-a", Namespaces: []string{"a1", "a2"}, Capability: access.CapView},
		{Project: "team-b", Namespaces: []string{"b1"}, Capability: access.CapView},
	}}}

	// No filter -> union of all.
	got, _ := handleListNamespaces(stub)(ctxUser("alice"), nil, args{})
	if n := got.(map[string]any)["count"]; n != 3 {
		t.Errorf("union count = %v, want 3", n)
	}
	// Project filter -> just that project's namespaces.
	got, _ = handleListNamespaces(stub)(ctxUser("alice"), nil, args{"project": "team-a"})
	if n := got.(map[string]any)["count"]; n != 2 {
		t.Errorf("filtered count = %v, want 2", n)
	}
}
