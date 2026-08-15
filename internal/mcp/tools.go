package mcp

import "github.com/snapp-incubator/argocd-mcp/internal/access"

// Schema helpers: JSON-Schema fragments for tool inputs.

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// nsDesc documents the tenant-scoping argument shared by every tool. It is the
// destination namespace (the team's own namespace), never "user-argocd".
const nsDesc = "The team / destination namespace: the namespace Applications deploy into " +
	"(spec.destination.namespace), which equals the team's own namespace and its " +
	"AppProject name. NOT the shared 'user-argocd' namespace where the CRs live."

func buildTools(r access.Resolver) []tool {
	return []tool{
		{
			name: "argocd_list_applications",
			description: "List ArgoCD Applications that deploy into a namespace, with project, " +
				"sync status, health, and source (repo/path/revision). Start here for " +
				"\"what are my apps\" or \"are my deployments healthy\".",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"project":   str("Optional: only Applications in this AppProject."),
			}, "namespace"),
			handler: handleListApplications,
		},
		{
			name: "argocd_get_application",
			description: "Full read-only view of one ArgoCD Application deploying into the given " +
				"namespace: sync/health, source, sync-policy, conditions, the last sync " +
				"operation, and the per-resource sync/health list. Use to diagnose an app " +
				"that is OutOfSync or Degraded.",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
				"name":      str("Application name."),
			}, "namespace", "name"),
			handler: handleGetApplication,
		},
		{
			name: "argocd_get_appproject",
			description: "Describe the AppProject governing a namespace: allowed source repos, " +
				"destinations, and roles (which groups hold which permissions). Answers " +
				"\"who can access my project\" and \"where can it deploy from\".",
			schema: objSchema(map[string]any{
				"namespace": str(nsDesc),
			}, "namespace"),
			handler: handleGetAppProject,
		},

		// --- Identity-scoped tools (no namespace argument) ---
		// These answer "what can *I* access" from the caller's identity (the
		// X-Remote-User header set by the bot). They resolve the caller's groups
		// and match them against AppProject roles, so they honor ArgoCD's own
		// group RBAC. The bot marks them self-authorized (returned unfiltered).
		{
			name: "argocd_my_projects",
			description: "List every AppProject the CALLER can access, each with their capability " +
				"(admin / sync / view), destination namespaces, and the roles that grant it. " +
				"Answers \"which projects can I manage/admin/view?\". Needs no namespace argument.",
			schema:  objSchema(map[string]any{}),
			handler: handleMyProjects(r),
		},
		{
			name: "argocd_my_applications",
			description: "List every Application across all projects the CALLER can access, with " +
				"project, destination namespace, sync, health, and the caller's capability on it. " +
				"Answers \"list all my apps / which of my apps are unhealthy?\". No namespace argument.",
			schema:  objSchema(map[string]any{}),
			handler: handleMyApplications(r),
		},
		{
			name: "argocd_my_namespaces",
			description: "List the destination namespaces the CALLER's accessible projects deploy " +
				"into. Answers \"which namespaces can I access in ArgoCD?\". No namespace argument.",
			schema:  objSchema(map[string]any{}),
			handler: handleMyNamespaces(r),
		},
		{
			name: "argocd_can_i",
			description: "Check whether the CALLER may perform an action on an Application (e.g. " +
				"action=sync, get, delete, override, action/apps/Deployment/restart). Returns " +
				"allowed true/false with the reason, from the caller's ArgoCD role policies.",
			schema: objSchema(map[string]any{
				"application": str("Application name to check."),
				"action":      str("Action to check, e.g. sync, get, delete, override, or action/apps/Deployment/restart."),
			}, "application", "action"),
			handler: handleCanI(r),
		},
	}
}
