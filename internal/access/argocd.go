package access

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/snapp-incubator/argocd-mcp/internal/groups"
)

var (
	projGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "appprojects"}
	cmGVR   = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
)

// argocdResolver implements Resolver by matching a user's OpenShift groups
// against AppProject roles and the global argocd-rbac-cm — exactly how ArgoCD
// itself authorizes. The group lookup is delegated to a groups.Resolver so that
// specific step is independently swappable.
type argocdResolver struct {
	groups groups.Resolver
	dyn    dynamic.Interface
	ns     string // argocd namespace (where AppProjects + rbac-cm live)
	rbacCM string // rbac ConfigMap name (argocd-rbac-cm)
}

// NewArgoCD builds the OpenShift/ArgoCD access resolver.
func NewArgoCD(gr groups.Resolver, dyn dynamic.Interface, argocdNS, rbacCM string) Resolver {
	if argocdNS == "" {
		argocdNS = "user-argocd"
	}
	if rbacCM == "" {
		rbacCM = "argocd-rbac-cm"
	}
	return &argocdResolver{groups: gr, dyn: dyn, ns: argocdNS, rbacCM: rbacCM}
}

func (r *argocdResolver) AccessFor(ctx context.Context, user string) (UserAccess, error) {
	userGroups, err := r.groups.GroupsFor(ctx, user)
	if err != nil {
		return UserAccess{}, fmt.Errorf("resolve groups: %w", err)
	}
	groupSet := toSet(userGroups)

	global := r.globalCapability(ctx, groupSet) // admin | view | none, from rbac-cm

	list, err := r.dyn.Resource(projGVR).Namespace(r.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return UserAccess{}, fmt.Errorf("list appprojects: %w", err)
	}

	out := UserAccess{User: user}
	for i := range list.Items {
		p := &list.Items[i]
		pa := projectAccess(p, groupSet, global)
		if pa.Capability == CapNone {
			continue
		}
		out.Projects = append(out.Projects, pa)
	}
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].Project < out.Projects[j].Project })
	return out, nil
}

// projectAccess computes the user's access to one AppProject: the roles whose
// groups intersect the user's, their applications policies, the derived
// capability, and any global (rbac-cm) grant applied on top.
func projectAccess(p *unstructured.Unstructured, userGroups map[string]bool, global Capability) ProjectAccess {
	name := p.GetName()
	pa := ProjectAccess{Project: name, Namespaces: destinationNamespaces(p)}

	roles, _, _ := unstructured.NestedSlice(p.Object, "spec", "roles")
	for _, r := range roles {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if !intersects(stringSlice(rm["groups"]), userGroups) {
			continue
		}
		pa.Roles = append(pa.Roles, str(rm["name"]))
		for _, line := range stringSlice(rm["policies"]) {
			if pol, ok := parsePolicy(line); ok {
				pa.Policies = append(pa.Policies, pol)
			}
		}
	}
	pa.Capability = DeriveCapability(pa.Policies)

	// Global grant (argocd-admin -> admin everywhere, sre-team-view -> view
	// everywhere) applies on top and synthesizes a policy so can-i still works
	// for projects the user reaches only via the global role.
	if global != CapNone && global.rank() > pa.Capability.rank() {
		pa.Capability = global
		action := "get"
		if global == CapAdmin {
			action = "*"
		}
		pa.Policies = append(pa.Policies, Policy{Resource: "applications", Action: action, Object: name + "/*"})
	}
	sort.Strings(pa.Roles)
	return pa
}

// globalCapability resolves the cluster-wide role a user's groups map to via the
// argocd-rbac-cm policy.csv (g-lines) and policy.default. role:admin -> admin,
// role:readonly -> view, anything else (role:common, none) -> no app access.
func (r *argocdResolver) globalCapability(ctx context.Context, userGroups map[string]bool) Capability {
	cm, err := r.dyn.Resource(cmGVR).Namespace(r.ns).Get(ctx, r.rbacCM, metav1.GetOptions{})
	if err != nil {
		return CapNone // fail-closed: no global grant
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	edges := parseGrouping(data["policy.csv"]) // subject -> roles

	best := CapNone
	for g := range userGroups {
		best = Max(best, capabilityOfRole(reachableRoles(g, edges)))
	}
	best = Max(best, capabilityOfRole(map[string]bool{strings.TrimSpace(data["policy.default"]): true}))
	return best
}

// --- parsing helpers ---

// parsePolicy parses a Casbin `p` line: "p, <sub>, <resource>, <action>,
// <object>, <effect>". Returns the (resource, action, object) for allow rules.
func parsePolicy(line string) (Policy, bool) {
	f := splitCSV(line)
	if len(f) < 5 || f[0] != "p" {
		return Policy{}, false
	}
	if len(f) >= 6 && strings.EqualFold(f[5], "deny") {
		return Policy{}, false // model allow-only
	}
	return Policy{Resource: f[2], Action: f[3], Object: f[4]}, true
}

// parseGrouping parses Casbin `g` lines into subject -> [roles].
func parseGrouping(csv string) map[string][]string {
	edges := map[string][]string{}
	for _, line := range strings.Split(csv, "\n") {
		f := splitCSV(line)
		if len(f) >= 3 && f[0] == "g" {
			edges[f[1]] = append(edges[f[1]], f[2])
		}
	}
	return edges
}

// reachableRoles returns every role reachable from subject through g-edges
// (transitive, cycle-safe).
func reachableRoles(subject string, edges map[string][]string) map[string]bool {
	seen := map[string]bool{}
	var walk func(s string)
	walk = func(s string) {
		for _, t := range edges[s] {
			if !seen[t] {
				seen[t] = true
				walk(t)
			}
		}
	}
	walk(subject)
	return seen
}

func capabilityOfRole(roles map[string]bool) Capability {
	switch {
	case roles["role:admin"]:
		return CapAdmin
	case roles["role:readonly"]:
		return CapView
	default:
		return CapNone
	}
}

func destinationNamespaces(p *unstructured.Unstructured) []string {
	dests, _, _ := unstructured.NestedSlice(p.Object, "spec", "destinations")
	seen := map[string]bool{}
	var out []string
	for _, d := range dests {
		if dm, ok := d.(map[string]any); ok {
			ns := str(dm["namespace"])
			if ns != "" && !seen[ns] {
				seen[ns] = true
				out = append(out, ns)
			}
		}
	}
	sort.Strings(out)
	return out
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func stringSlice(v any) []string {
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

func str(v any) string { s, _ := v.(string); return s }

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func intersects(candidates []string, set map[string]bool) bool {
	for _, c := range candidates {
		if set[c] {
			return true
		}
	}
	return false
}
