package access

import "testing"

func TestDeriveCapability(t *testing.T) {
	cases := []struct {
		name     string
		policies []Policy
		want     Capability
	}{
		{"admin", []Policy{{"applications", "*", "dba/*"}}, CapAdmin},
		{"view", []Policy{{"applications", "get", "dba/*"}}, CapView},
		{"sync", []Policy{
			{"applications", "get", "dba/*"},
			{"applications", "sync", "dba/*"},
			{"applications", "action/apps/Deployment/restart", "dba/*"},
		}, CapSync},
		{"repositories-only-is-none", []Policy{{"repositories", "get", "dba/*"}}, CapNone},
		{"none", nil, CapNone},
	}
	for _, c := range cases {
		if got := DeriveCapability(c.policies); got != c.want {
			t.Errorf("%s: DeriveCapability = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAllows(t *testing.T) {
	adminPolicies := []Policy{{"applications", "*", "dba/*"}}
	syncPolicies := []Policy{
		{"applications", "get", "dba/*"},
		{"applications", "sync", "dba/*"},
		{"applications", "action/apps/Deployment/restart", "dba/*"},
	}

	cases := []struct {
		name     string
		policies []Policy
		action   string
		object   string
		want     bool
	}{
		{"admin can sync", adminPolicies, "sync", "dba/my-app", true},
		{"admin can delete", adminPolicies, "delete", "dba/my-app", true},
		{"admin cannot reach other project", adminPolicies, "get", "routing/x", false},
		{"sync can sync", syncPolicies, "sync", "dba/my-app", true},
		{"sync can restart (action tree)", syncPolicies, "action/apps/Deployment/restart", "dba/my-app", true},
		{"sync cannot delete", syncPolicies, "delete", "dba/my-app", false},
		{"sync can get", syncPolicies, "get", "dba/my-app", true},
	}
	for _, c := range cases {
		if got := Allows(c.policies, "applications", c.action, c.object); got != c.want {
			t.Errorf("%s: Allows(%q,%q) = %v, want %v", c.name, c.action, c.object, got, c.want)
		}
	}
}

func TestUserAccessHelpers(t *testing.T) {
	ua := UserAccess{User: "a@x.com", Projects: []ProjectAccess{
		{Project: "dba", Namespaces: []string{"dba"}, Capability: CapAdmin},
		{Project: "routing", Namespaces: []string{"routing", "*"}, Capability: CapView},
	}}
	if p, ok := ua.Project("dba"); !ok || p.Capability != CapAdmin {
		t.Errorf("Project(dba) = %+v, %v", p, ok)
	}
	if _, ok := ua.Project("nope"); ok {
		t.Error("Project(nope) should be false")
	}
	ns := ua.Namespaces()
	if len(ns) != 2 || ns[0] != "dba" || ns[1] != "routing" { // "*" filtered out, sorted
		t.Errorf("Namespaces() = %v, want [dba routing]", ns)
	}
}
