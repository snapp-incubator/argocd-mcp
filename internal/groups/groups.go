// Package groups resolves a user's group memberships. This is deliberately kept
// behind a small interface so the backing source can be swapped later (today:
// OpenShift `user.openshift.io` Group objects; tomorrow: an IAM/directory
// service) without touching the ArgoCD access logic that consumes it.
package groups

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Resolver returns the group identifiers a user belongs to. The identifiers must
// match whatever the consumer compares against (for ArgoCD, the names in an
// AppProject role's `groups` list). Implementations should be safe for
// concurrent use.
type Resolver interface {
	// GroupsFor returns the groups the user (an email / SSO username) belongs to.
	// It returns an empty slice (not an error) for an unknown user.
	GroupsFor(ctx context.Context, user string) ([]string, error)
}

// ImplicitGroups are attached to every authenticated user (OpenShift includes
// them in `oc auth can-i --as=<user>`), so RBAC that binds them is honored.
// Exported so tests and alternative backends can reuse them.
var ImplicitGroups = []string{"system:authenticated", "system:authenticated:oauth"}

var groupGVR = schema.GroupVersionResource{
	Group: "user.openshift.io", Version: "v1", Resource: "groups",
}

// openshift resolves groups from the cluster's `user.openshift.io` Group
// objects (the same source ArgoCD's Dex OpenShift connector reads at login),
// mapping user -> groups, refreshed on a short TTL. This is live — unlike a
// token's snapshot it reflects membership changes without re-login.
type openshift struct {
	dyn dynamic.Interface
	ttl time.Duration

	mu      sync.Mutex
	expires time.Time
	byUser  map[string][]string
}

// NewOpenShift builds a Resolver backed by OpenShift Group objects, caching the
// user->groups map for ttl.
func NewOpenShift(dyn dynamic.Interface, ttl time.Duration) Resolver {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &openshift{dyn: dyn, ttl: ttl, byUser: map[string][]string{}}
}

func (o *openshift) GroupsFor(ctx context.Context, user string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if time.Now().After(o.expires) {
		m, err := o.load(ctx)
		if err != nil {
			// Serve stale data on transient errors rather than dropping all
			// access; only fail hard if we have never loaded successfully.
			if len(o.byUser) == 0 {
				return nil, err
			}
		} else {
			o.byUser = m
		}
		// Back off even on error so we don't hammer the API server.
		o.expires = time.Now().Add(o.ttl)
	}
	return withImplicit(o.byUser[user]), nil
}

// load lists Group objects and builds user -> [group names].
func (o *openshift) load(ctx context.Context) (map[string][]string, error) {
	list, err := o.dyn.Resource(groupGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return ParseGroups(list.Items), nil
}

// ParseGroups builds a user -> group-names map from Group objects. Exported so
// it can be unit-tested without a cluster.
func ParseGroups(items []unstructured.Unstructured) map[string][]string {
	m := map[string][]string{}
	for i := range items {
		g := &items[i]
		name := g.GetName()
		users, _, _ := unstructured.NestedStringSlice(g.Object, "users")
		for _, u := range users {
			m[u] = append(m[u], name)
		}
	}
	return m
}

// withImplicit appends the implicit authenticated groups, de-duplicated.
func withImplicit(named []string) []string {
	out := make([]string, 0, len(named)+len(ImplicitGroups))
	seen := map[string]bool{}
	for _, g := range named {
		if g != "" && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	for _, g := range ImplicitGroups {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}
