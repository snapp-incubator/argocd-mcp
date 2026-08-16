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

// nsMeaning documents what a "namespace" is across the tools: the destination
// namespace (the team's own namespace), never "user-argocd" where the CRs live.
const nsMeaning = "the destination namespace Applications deploy into " +
	"(spec.destination.namespace) — the team's own namespace, which usually equals its " +
	"AppProject name. NOT the shared 'user-argocd' namespace where the CRs live."

// buildTools returns the tool set. Every tool is IDENTITY-scoped: it authorizes
// from the caller's X-Remote-User identity (resolved to OpenShift groups matched
// against AppProject roles) and answers "what can *I* access". Any
// namespace/project/name argument is only an optional filter or selector,
// validated against that access — never the authorization mechanism.
func buildTools(r access.Resolver) []tool {
	return []tool{
		{
			name: "argocd_list_applications",
			description: "List the CALLER's ArgoCD Applications (across every project they can access), " +
				"each with project, destination namespace, sync status, health, source, and the " +
				"caller's capability. Start here for \"what are my apps / are they healthy?\". " +
				"Optional filters narrow the list.",
			schema: objSchema(map[string]any{
				"namespace": str("Optional filter: only apps deploying into this namespace (" + nsMeaning + ")."),
				"project":   str("Optional filter: only apps in this AppProject."),
			}),
			handler: handleListApplications(r),
		},
		{
			name: "argocd_get_application",
			description: "Full read-only view of ONE of the caller's Applications: sync/health, source, " +
				"conditions, the last sync operation, and the per-resource sync/health list. Use to " +
				"diagnose an app that is OutOfSync or Degraded. Returns not-found if the caller has no " +
				"access to it.",
			schema: objSchema(map[string]any{
				"name": str("Application name."),
				"namespace": str("Optional: the destination namespace, to DISAMBIGUATE apps that share a name " +
					"across namespaces (" + nsMeaning + "). Omit when the name is unique."),
			}, "name"),
			handler: handleGetApplication(r),
		},
		{
			name: "argocd_list_projects",
			description: "List the projects the CALLER can access as summaries: project, capability " +
				"(admin / sync / view), destination namespaces, and role names. Answers \"which projects " +
				"can I manage/admin/view?\". Optional namespace narrows to projects governing it; drill " +
				"into one with argocd_get_project.",
			schema: objSchema(map[string]any{
				"namespace": str("Optional filter: only projects that deploy into this namespace (" + nsMeaning + ")."),
			}),
			handler: handleListProjects(r),
		},
		{
			name: "argocd_get_project",
			description: "Full detail of a project the CALLER can access: allowed source repos, " +
				"destinations, roles (which groups hold which permissions), and the caller's capability. " +
				"Select by name (one project) OR by namespace (EVERY project whose destinations include it " +
				"— a namespace may be governed by more than one). Answers \"who can access my project\" " +
				"and \"where can it deploy from\". Provide name or namespace.",
			schema: objSchema(map[string]any{
				"name":      str("The AppProject name. Provide this OR namespace."),
				"namespace": str("A destination namespace (" + nsMeaning + "); returns every accessible project that deploys into it. Provide this OR name."),
			}),
			handler: handleGetProject(r),
		},
		{
			name: "argocd_list_namespaces",
			description: "List the destination namespaces the CALLER's accessible projects deploy into. " +
				"Answers \"which namespaces can I access in ArgoCD?\". Optional project narrows to that " +
				"one project's destinations.",
			schema: objSchema(map[string]any{
				"project": str("Optional filter: only the destination namespaces of this AppProject."),
			}),
			handler: handleListNamespaces(r),
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
