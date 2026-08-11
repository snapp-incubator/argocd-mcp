package mcp

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

func buildTools() []tool {
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
	}
}
