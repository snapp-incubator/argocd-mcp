// Package mcp implements the read-only ArgoCD MCP server. Its tools read the
// argoproj.io Application and AppProject custom resources from the Kubernetes
// API (never the ArgoCD API) and summarize them for an AI agent. Every record
// is keyed by the object's DESTINATION namespace — the tenant's own namespace —
// and never by the shared argocd namespace, so a namespace-scoped caller (the
// SnappCloud bot) can filter results per team. Nothing here mutates state.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/snapp-incubator/argocd-mcp/internal/k8s"
	"github.com/snapp-incubator/argocd-mcp/internal/version"
)

// args is a decoded tool-call argument map with typed getters.
type args map[string]any

func (a args) str(key string) string {
	v, _ := a[key].(string)
	return v
}

// handlerFunc is a tool handler. It returns any JSON-marshalable value (or a
// plain string, passed through as-is). Errors become MCP error results.
type handlerFunc func(ctx context.Context, c *k8s.Client, a args) (any, error)

// tool couples a definition with its handler.
type tool struct {
	name        string
	description string
	schema      map[string]any
	handler     handlerFunc
}

// Server is the MCP server over a Kubernetes client.
type Server struct {
	mcpServer *sdkmcp.Server
	client    *k8s.Client
	log       *slog.Logger
}

// NewServer builds the server and registers all tools.
func NewServer(client *k8s.Client, log *slog.Logger) *Server {
	s := &Server{client: client, log: log}
	s.mcpServer = sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "argocd-mcp", Version: version.String()},
		&sdkmcp.ServerOptions{Instructions: instructions},
	)
	for _, t := range buildTools() {
		s.addTool(t)
	}
	return s
}

// addTool registers one tool, wrapping its handler with argument decoding,
// logging, and JSON rendering of results.
func (s *Server) addTool(t tool) {
	handler := t.handler
	s.mcpServer.AddTool(
		&sdkmcp.Tool{Name: t.name, Description: t.description, InputSchema: t.schema},
		func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var a args
			if len(req.Params.Arguments) > 0 {
				_ = json.Unmarshal(req.Params.Arguments, &a)
			}

			start := time.Now()
			s.log.Info("tool call", "tool", t.name, "args", map[string]any(a))

			out, err := handler(ctx, s.client, a)
			duration := time.Since(start)
			if err != nil {
				s.log.Warn("tool error", "tool", t.name, "duration", duration, "err", err)
				return errResult("%v", err), nil
			}

			text, ok := out.(string)
			if !ok {
				b, merr := json.MarshalIndent(out, "", "  ")
				if merr != nil {
					return errResult("marshal result: %v", merr), nil
				}
				text = string(b)
			}
			s.log.Info("tool result", "tool", t.name, "duration", duration, "bytes", len(text))
			return textResult(text), nil
		},
	)
}

// RunStdio serves MCP over stdin/stdout.
func (s *Server) RunStdio(ctx context.Context) error {
	return s.mcpServer.Run(ctx, &sdkmcp.StdioTransport{})
}

// HTTPHandler serves MCP over streamable HTTP. Stateless: these tools are pure
// request/response, so no per-client session state accumulates.
func (s *Server) HTTPHandler() http.Handler {
	return sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return s.mcpServer },
		&sdkmcp.StreamableHTTPOptions{Stateless: true},
	)
}

func textResult(text string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text}}}
}

func errResult(format string, a ...any) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: fmt.Sprintf(format, a...)}},
		IsError: true,
	}
}

const instructions = `You are a read-only ArgoCD assistant. Every tool reads ArgoCD Applications and
AppProjects and reports sync/health, sources, and per-resource status. You never
change anything — you explain state and recommend actions the user can take.

Scope: every tool takes a "namespace" argument that is the TEAM / DESTINATION
namespace — the namespace an Application deploys into (spec.destination.namespace),
which for a team equals its own namespace and its AppProject name. It is NOT the
shared "user-argocd" namespace where the CRs live. Always pass the user's own
namespace.

Workflows:
1. "What are my ArgoCD apps / are they healthy?": argocd_list_applications
   (namespace=<team ns>) → each app's sync (Synced/OutOfSync) and health
   (Healthy/Degraded/Progressing/Missing).
2. "Why is <app> unhealthy / out of sync?": argocd_get_application
   (namespace=<team ns>, name=<app>) → conditions, last operation, and the
   per-resource sync/health list; a Degraded resource or a failed sync operation
   is the lead.
3. "Who can access my project / where can it deploy from?": argocd_get_appproject
   (namespace=<team ns>) → source repos, destinations, and roles (which groups
   have which permissions).

Tips:
- sync=OutOfSync means live state drifted from Git; health=Degraded means the
  workload itself is unhealthy — they are independent, check both.
- Summaries omit healthy detail; use argocd_get_application for the full picture
  of one app.`
