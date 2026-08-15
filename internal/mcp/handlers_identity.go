package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/argocd-mcp/internal/access"
	"github.com/snapp-incubator/argocd-mcp/internal/k8s"
)

// Identity-scoped handlers. Each resolves the CALLER's ArgoCD access from the
// X-Remote-User identity (set by the trusted bot, read from ctx) and answers
// without a namespace argument. They never take identity from tool arguments.

func errIdentityRequired() error {
	return fmt.Errorf("identity required: the caller must be identified via the %s header", identityHeader)
}

func handleMyProjects(r access.Resolver) handlerFunc {
	return func(ctx context.Context, _ *k8s.Client, _ args) (any, error) {
		user := userFrom(ctx)
		if user == "" {
			return nil, errIdentityRequired()
		}
		ua, err := r.AccessFor(ctx, user)
		if err != nil {
			return nil, err
		}
		return map[string]any{"user": user, "count": len(ua.Projects), "projects": ua.Projects}, nil
	}
}

func handleMyNamespaces(r access.Resolver) handlerFunc {
	return func(ctx context.Context, _ *k8s.Client, _ args) (any, error) {
		user := userFrom(ctx)
		if user == "" {
			return nil, errIdentityRequired()
		}
		ua, err := r.AccessFor(ctx, user)
		if err != nil {
			return nil, err
		}
		ns := ua.Namespaces()
		return map[string]any{"user": user, "count": len(ns), "namespaces": ns}, nil
	}
}

func handleMyApplications(r access.Resolver) handlerFunc {
	return func(ctx context.Context, c *k8s.Client, _ args) (any, error) {
		user := userFrom(ctx)
		if user == "" {
			return nil, errIdentityRequired()
		}
		ua, err := r.AccessFor(ctx, user)
		if err != nil {
			return nil, err
		}
		list, err := c.Dynamic.Resource(appGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list applications: %w", err)
		}
		out := make([]map[string]any, 0)
		for i := range list.Items {
			item := &list.Items[i]
			pa, ok := ua.Project(appProject(item))
			if !ok {
				continue
			}
			s := applicationSummary(item)
			s["capability"] = string(pa.Capability)
			out = append(out, s)
		}
		sort.Slice(out, func(i, j int) bool { return name(out[i]) < name(out[j]) })
		return map[string]any{"user": user, "count": len(out), "applications": out}, nil
	}
}

func handleCanI(r access.Resolver) handlerFunc {
	return func(ctx context.Context, c *k8s.Client, a args) (any, error) {
		user := userFrom(ctx)
		if user == "" {
			return nil, errIdentityRequired()
		}
		app, action := a.str("application"), a.str("action")
		if app == "" || action == "" {
			return nil, fmt.Errorf("both application and action are required")
		}
		ua, err := r.AccessFor(ctx, user)
		if err != nil {
			return nil, err
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
