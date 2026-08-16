package mcp

import (
	"context"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/argocd-mcp/internal/access"
	"github.com/snapp-incubator/argocd-mcp/internal/k8s"
)

// Identity-scoped handlers. Every tool resolves the CALLER's ArgoCD access from
// the X-Remote-User identity (set by the trusted bot, read from ctx) and answers
// "what can *I* access". Identity is never taken from tool arguments; any
// namespace/project/name argument is only an optional filter or selector.

func errIdentityRequired() error {
	return fmt.Errorf("identity required: the caller must be identified via the %s header", identityHeader)
}

// callerAccess resolves the caller's identity from ctx and their ArgoCD access,
// refusing when no identity is present. Every handler starts here, so the model
// can never reach data outside the caller's access.
func callerAccess(ctx context.Context, r access.Resolver) (string, access.UserAccess, error) {
	user := userFrom(ctx)
	if user == "" {
		return "", access.UserAccess{}, errIdentityRequired()
	}
	ua, err := r.AccessFor(ctx, user)
	return user, ua, err
}

// handleListProjects is the lightweight browse: the caller's accessible projects
// as summaries (project, capability, namespaces, role names). Drill into one
// with argocd_get_project. Optional namespace narrows to projects governing it.
func handleListProjects(r access.Resolver) handlerFunc {
	return func(ctx context.Context, _ *k8s.Client, a args) (any, error) {
		user, ua, err := callerAccess(ctx, r)
		if err != nil {
			return nil, err
		}
		nsFilter := a.str("namespace")
		out := make([]access.ProjectAccess, 0, len(ua.Projects))
		for _, p := range ua.Projects {
			if nsFilter != "" && !contains(p.Namespaces, nsFilter) {
				continue
			}
			out = append(out, p)
		}
		return map[string]any{"user": user, "count": len(out), "projects": out}, nil
	}
}

// handleGetProject is the detail view: full source_repos, destinations, and
// roles for a project the caller can access. Select by name (one project) OR by
// namespace (every project whose destinations include it — possibly several).
func handleGetProject(r access.Resolver) handlerFunc {
	return func(ctx context.Context, c *k8s.Client, a args) (any, error) {
		user, ua, err := callerAccess(ctx, r)
		if err != nil {
			return nil, err
		}
		nameSel, nsSel := a.str("name"), a.str("namespace")
		if nameSel == "" && nsSel == "" {
			return nil, fmt.Errorf("provide name (a specific project) or namespace (projects governing it)")
		}

		list, err := c.Dynamic.Resource(projGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list appprojects: %w", err)
		}
		out := make([]map[string]any, 0, 1)
		for i := range list.Items {
			item := &list.Items[i]
			if nameSel != "" && item.GetName() != nameSel {
				continue
			}
			// A namespace is governed by every project that lists it as a
			// destination — collect them all, not just the first.
			if nsSel != "" && item.GetName() != nsSel && !projectTargetsNamespace(item, nsSel) {
				continue
			}
			pa, ok := ua.Project(item.GetName())
			if !ok {
				continue // caller cannot access this project — omit (no leak)
			}
			d := projectDetail(item)
			d["capability"] = string(pa.Capability)
			out = append(out, d)
		}
		return map[string]any{"user": user, "count": len(out), "projects": out}, nil
	}
}

// handleListNamespaces lists the destination namespaces the caller can reach.
// Optional project narrows to that one project's destinations.
func handleListNamespaces(r access.Resolver) handlerFunc {
	return func(ctx context.Context, _ *k8s.Client, a args) (any, error) {
		user, ua, err := callerAccess(ctx, r)
		if err != nil {
			return nil, err
		}
		var ns []string
		if projSel := a.str("project"); projSel != "" {
			if pa, ok := ua.Project(projSel); ok {
				// Reuse UserAccess.Namespaces' dedupe/clean over a single project.
				ns = access.UserAccess{Projects: []access.ProjectAccess{pa}}.Namespaces()
			}
		} else {
			ns = ua.Namespaces()
		}
		if ns == nil {
			ns = []string{}
		}
		return map[string]any{"user": user, "count": len(ns), "namespaces": ns}, nil
	}
}

func handleCanI(r access.Resolver) handlerFunc {
	return func(ctx context.Context, c *k8s.Client, a args) (any, error) {
		_, ua, err := callerAccess(ctx, r)
		if err != nil {
			return nil, err
		}
		app, action := a.str("application"), a.str("action")
		if app == "" || action == "" {
			return nil, fmt.Errorf("both application and action are required")
		}

		// Resolve the application -> its project.
		list, err := c.Dynamic.Resource(appGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list applications: %w", err)
		}
		project := ""
		for i := range list.Items {
			if list.Items[i].GetName() == app {
				project = appProject(&list.Items[i])
				break
			}
		}

		deny := func(reason string) any {
			return map[string]any{"allowed": false, "application": app, "action": action, "reason": reason}
		}
		if project == "" {
			// Do not distinguish "missing" from "inaccessible" — avoids enumeration.
			return deny("application not found or you do not have access to it"), nil
		}
		pa, ok := ua.Project(project)
		if !ok {
			return deny("you do not have access to this application's project"), nil
		}
		allowed := access.Allows(pa.Policies, "applications", action, project+"/"+app)
		reason := fmt.Sprintf("your capability on project %q is %q", project, pa.Capability)
		if !allowed {
			reason = fmt.Sprintf("action %q is not permitted by your %q role on project %q", action, pa.Capability, project)
		}
		return map[string]any{
			"allowed":     allowed,
			"application": app,
			"project":     project,
			"action":      action,
			"capability":  string(pa.Capability),
			"reason":      reason,
		}, nil
	}
}

// --- identity-scoped config ---

func groupCacheTTL() time.Duration {
	if v := os.Getenv("GROUP_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return time.Minute
}

func rbacConfigMap() string {
	if v := os.Getenv("ARGOCD_RBAC_CONFIGMAP"); v != "" {
		return v
	}
	return "argocd-rbac-cm"
}
