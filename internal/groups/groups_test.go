package groups

import (
	"reflect"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func group(name string, users ...string) unstructured.Unstructured {
	us := make([]any, len(users))
	for i, u := range users {
		us[i] = u
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "user.openshift.io/v1",
		"kind":       "Group",
		"metadata":   map[string]any{"name": name},
		"users":      us,
	}}
}

func TestParseGroups(t *testing.T) {
	items := []unstructured.Unstructured{
		group("dba-admin", "a@x.com", "b@x.com"),
		group("dba-view", "b@x.com", "c@x.com"),
		group("routing-admin", "d@x.com"),
	}
	got := ParseGroups(items)

	want := map[string][]string{
		"a@x.com": {"dba-admin"},
		"b@x.com": {"dba-admin", "dba-view"},
		"c@x.com": {"dba-view"},
		"d@x.com": {"routing-admin"},
	}
	for u, w := range want {
		sort.Strings(got[u])
		sort.Strings(w)
		if !reflect.DeepEqual(got[u], w) {
			t.Errorf("groups for %s = %v, want %v", u, got[u], w)
		}
	}
}

func TestWithImplicit(t *testing.T) {
	got := withImplicit([]string{"dba-admin", "dba-admin"}) // dup input
	// named first (deduped), then implicit, no duplicates.
	want := []string{"dba-admin", "system:authenticated", "system:authenticated:oauth"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("withImplicit = %v, want %v", got, want)
	}
	// An empty membership still yields the implicit groups.
	if g := withImplicit(nil); !reflect.DeepEqual(g, ImplicitGroups) {
		t.Errorf("withImplicit(nil) = %v, want %v", g, ImplicitGroups)
	}
}
