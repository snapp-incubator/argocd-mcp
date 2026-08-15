package access

import (
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// dbaProject builds the AppProject from the real `dba` example (admin/view/sync
// roles bound to OpenShift groups), as an unstructured object.
func dbaProject() *unstructured.Unstructured {
	role := func(name string, groups []string, policies []string) map[string]any {
		gs := make([]any, len(groups))
		for i, g := range groups {
			gs[i] = g
		}
		ps := make([]any, len(policies))
		for i, p := range policies {
			ps[i] = p
		}
		return map[string]any{"name": name, "groups": gs, "policies": ps}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "AppProject",
		"metadata":   map[string]any{"name": "dba", "namespace": "user-argocd"},
		"spec": map[string]any{
			"destinations": []any{map[string]any{"namespace": "dba", "server": "*"}},
			"roles": []any{
				role("dba-admin", []string{"dba-admin", "dba-admin-ci"}, []string{
					"p, proj:dba:dba-admin, applications, *, dba/*, allow",
					"p, proj:dba:dba-admin, repositories, *, dba/*, allow",
					"p, proj:dba:dba-admin, exec, create, dba/*, allow",
				}),
				role("dba-view", []string{"dba-admin", "dba-admin-ci", "dba-view", "dba-view-ci"}, []string{
					"p, proj:dba:dba-view, applications, get, dba/*, allow",
					"p, proj:dba:dba-view, logs, get, dba/*, allow",
				}),
				role("dba-sync", []string{"dba-sync", "dba-sync-ci"}, []string{
					"p, proj:dba:dba-sync, applications, get, dba/*, allow",
					"p, proj:dba:dba-sync, applications, sync, dba/*, allow",
					"p, proj:dba:dba-sync, applications, action/apps/Deployment/restart, dba/*, allow",
				}),
			},
		},
	}}
}

func TestProjectAccess_Capabilities(t *testing.T) {
	cases := []struct {
		name   string
		groups []string
		want   Capability
		role   string // an expected role name in the result (or "")
	}{
		{"admin group", []string{"dba-admin"}, CapAdmin, "dba-admin"},
		{"admin-ci group", []string{"dba-admin-ci"}, CapAdmin, "dba-admin"},
		{"view group", []string{"dba-view"}, CapView, "dba-view"},
		{"sync group", []string{"dba-sync"}, CapSync, "dba-sync"},
		{"foreign group -> none", []string{"routing-admin"}, CapNone, ""},
		{"no groups -> none", nil, CapNone, ""},
	}
	for _, c := range cases {
		pa := projectAccess(dbaProject(), toSet(c.groups), CapNone)
		if pa.Capability != c.want {
			t.Errorf("%s: capability = %q, want %q", c.name, pa.Capability, c.want)
		}
		if c.role != "" && !contains(pa.Roles, c.role) {
			t.Errorf("%s: roles = %v, want to contain %q", c.name, pa.Roles, c.role)
		}
		if c.want != CapNone && (len(pa.Namespaces) != 1 || pa.Namespaces[0] != "dba") {
			t.Errorf("%s: namespaces = %v, want [dba]", c.name, pa.Namespaces)
		}
	}
}

func TestProjectAccess_GlobalOverride(t *testing.T) {
	// A user in no dba group, but globally admin (argocd-admin) -> admin on dba,
	// with a synthesized policy so can-i works.
	pa := projectAccess(dbaProject(), toSet([]string{"someone-else"}), CapAdmin)
	if pa.Capability != CapAdmin {
		t.Fatalf("global admin: capability = %q, want admin", pa.Capability)
	}
	if !Allows(pa.Policies, "applications", "sync", "dba/anything") {
		t.Error("global admin should be allowed to sync via synthesized policy")
	}
	// Global view does not downgrade an explicit admin.
	pa2 := projectAccess(dbaProject(), toSet([]string{"dba-admin"}), CapView)
	if pa2.Capability != CapAdmin {
		t.Errorf("explicit admin + global view = %q, want admin", pa2.Capability)
	}
}

func TestGlobalCapabilityParsing(t *testing.T) {
	csv := "p, role:common, clusters, get, *, allow\n" +
		"g, argocd-admin, role:admin\n" +
		"g, sre-team-view, role:readonly\n"
	edges := parseGrouping(csv)

	if got := capabilityOfRole(reachableRoles("argocd-admin", edges)); got != CapAdmin {
		t.Errorf("argocd-admin -> %q, want admin", got)
	}
	if got := capabilityOfRole(reachableRoles("sre-team-view", edges)); got != CapView {
		t.Errorf("sre-team-view -> %q, want view", got)
	}
	if got := capabilityOfRole(reachableRoles("dba-admin", edges)); got != CapNone {
		t.Errorf("dba-admin (no global) -> %q, want none", got)
	}
}

func TestParsePolicy(t *testing.T) {
	p, ok := parsePolicy("p, proj:dba:dba-admin, applications, *, dba/*, allow")
	if !ok || p.Resource != "applications" || p.Action != "*" || p.Object != "dba/*" {
		t.Fatalf("parsePolicy = %+v, ok=%v", p, ok)
	}
	if _, ok := parsePolicy("g, dba-admin, proj:dba:dba-admin"); ok {
		t.Error("g-line should not parse as a policy")
	}
	if _, ok := parsePolicy("p, s, applications, delete, dba/*, deny"); ok {
		t.Error("deny policy should be skipped (allow-only model)")
	}
}

func contains(ss []string, want string) bool {
	i := sort.SearchStrings(ss, want)
	return i < len(ss) && ss[i] == want
}
