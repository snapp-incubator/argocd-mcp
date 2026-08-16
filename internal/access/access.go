// Package access answers "what ArgoCD access does this user have?" behind a
// single interface (Resolver), so the whole computation can be swapped later —
// today it matches OpenShift groups against AppProject roles; a future backend
// (IAM, an external RBAC service) can compute the same UserAccess differently.
package access

import (
	"context"
	"path"
	"sort"
	"strings"
)

// Capability is a coarse label for what a user may do in a project, derived from
// the ArgoCD `applications` policies they hold.
type Capability string

const (
	CapNone  Capability = ""
	CapView  Capability = "view"  // read: get/logs
	CapSync  Capability = "sync"  // view + sync/rollback/restart
	CapAdmin Capability = "admin" // full control
)

// rank orders capabilities so max() can pick the strongest.
func (c Capability) rank() int {
	switch c {
	case CapAdmin:
		return 3
	case CapSync:
		return 2
	case CapView:
		return 1
	default:
		return 0
	}
}

// Max returns the stronger of two capabilities.
func Max(a, b Capability) Capability {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// Policy is a single ArgoCD allow-rule the user holds (a Casbin `p` line minus
// the `allow` effect). Kept structured so can-i can evaluate it precisely; a
// backend that has no Casbin policies may leave this empty and rely on
// Capability alone.
type Policy struct {
	Resource string // e.g. "applications", "repositories", "logs", "*"
	Action   string // e.g. "*", "get", "sync", "action/apps/Deployment/restart"
	Object   string // e.g. "dba/*"
}

// ProjectAccess is the user's access to one AppProject.
type ProjectAccess struct {
	Project    string     `json:"project"`
	Namespaces []string   `json:"namespaces"`      // destination namespaces
	Capability Capability `json:"capability"`      // admin | sync | view
	Roles      []string   `json:"roles,omitempty"` // role names that granted access
	Policies   []Policy   `json:"-"`               // effective allow-policies (for can-i)
}

// UserAccess is everything a user can reach across projects.
type UserAccess struct {
	User     string          `json:"user"`
	Projects []ProjectAccess `json:"projects"`
}

// Project returns the user's access to a named project, if any.
func (u UserAccess) Project(name string) (ProjectAccess, bool) {
	for _, p := range u.Projects {
		if p.Project == name {
			return p, true
		}
	}
	return ProjectAccess{}, false
}

// Namespaces returns the union of all destination namespaces the user can reach.
func (u UserAccess) Namespaces() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range u.Projects {
		for _, ns := range p.Namespaces {
			if ns != "" && ns != "*" && !seen[ns] {
				seen[ns] = true
				out = append(out, ns)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Resolver computes a user's ArgoCD access. THIS is the swap boundary: replace
// the implementation (e.g. with an IAM-backed one) without changing the tools.
type Resolver interface {
	// AccessFor returns the projects (and capability) the user can access. It
	// returns an empty UserAccess (not an error) for a user with no access.
	AccessFor(ctx context.Context, user string) (UserAccess, error)
}

// DeriveCapability classifies a set of `applications` policies into a coarse
// capability label. Derived from the actual policy verbs (not role-name
// convention), so custom roles are classified correctly.
func DeriveCapability(policies []Policy) Capability {
	cap := CapNone
	for _, p := range policies {
		if p.Resource != "applications" && p.Resource != "*" {
			continue
		}
		switch {
		case p.Action == "*":
			cap = Max(cap, CapAdmin)
		case p.Action == "sync", p.Action == "override", strings.HasPrefix(p.Action, "action/"):
			cap = Max(cap, CapSync)
		case p.Action == "get":
			cap = Max(cap, CapView)
		}
	}
	return cap
}

// Allows reports whether any policy permits (resource, action, object). Casbin's
// object globs (e.g. "dba/*") are matched with path.Match; "*" matches anything.
func Allows(policies []Policy, resource, action, object string) bool {
	for _, p := range policies {
		if globMatch(p.Resource, resource) && globMatch(p.Action, action) && globMatch(p.Object, object) {
			return true
		}
	}
	return false
}

// globMatch matches a Casbin-style pattern against a value: "*" matches
// anything; otherwise an exact match or a path.Match glob (so "dba/*" matches
// "dba/my-app" and "action/*" matches "action/apps/Deployment/restart"... note
// path.Match does not cross "/", so we also try a prefix form for action trees).
func globMatch(pattern, value string) bool {
	if pattern == "*" || pattern == value {
		return true
	}
	if ok, _ := path.Match(pattern, value); ok {
		return true
	}
	// A trailing "/*" should also cover deeper trees (action/apps/.../restart).
	if strings.HasSuffix(pattern, "/*") && strings.HasPrefix(value, strings.TrimSuffix(pattern, "*")) {
		return true
	}
	return false
}
