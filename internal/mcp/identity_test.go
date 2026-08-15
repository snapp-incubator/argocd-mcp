package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/snapp-incubator/argocd-mcp/internal/access"
)

// fakeResolver records the user it was asked about and returns a fixed access.
type fakeResolver struct{ sawUser string }

func (f *fakeResolver) AccessFor(_ context.Context, user string) (access.UserAccess, error) {
	f.sawUser = user
	return access.UserAccess{User: user, Projects: []access.ProjectAccess{
		{Project: "dba", Namespaces: []string{"dba"}, Capability: access.CapAdmin, Roles: []string{"dba-admin"}},
	}}, nil
}

// headerRT injects the identity header on every request (simulating the bot).
type headerRT struct {
	user string
	base http.RoundTripper
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.user != "" {
		r.Header.Set("X-Remote-User", h.user)
	}
	return h.base.RoundTrip(r)
}

// callMyProjects connects an SDK client (optionally injecting an identity
// header) and calls argocd_my_projects, returning the tool's text output.
func callMyProjects(t *testing.T, url, user string) (string, bool) {
	t.Helper()
	httpClient := &http.Client{Transport: headerRT{user: user, base: http.DefaultTransport}}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(context.Background(),
		&sdkmcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	res, err := sess.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "argocd_my_projects"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

// TestIdentityPropagation proves the x-remote-user header set by an upstream
// reaches the tool handler through the SDK (the crux of the identity design):
// with the header the resolver sees the user and returns their projects; without
// it the tool refuses.
func TestIdentityPropagation(t *testing.T) {
	fake := &fakeResolver{}
	srv := newServer(nil, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(WithIdentity(srv.HTTPHandler()))
	defer ts.Close()

	// With identity: resolver sees the user, output names their project.
	out, isErr := callMyProjects(t, ts.URL, "alice@example.com")
	if isErr {
		t.Fatalf("with identity: tool errored: %s", out)
	}
	if fake.sawUser != "alice@example.com" {
		t.Errorf("resolver saw user %q, want alice@example.com (header did not propagate)", fake.sawUser)
	}
	if !strings.Contains(out, "\"dba\"") || !strings.Contains(out, "admin") {
		t.Errorf("output missing expected project/capability: %s", out)
	}

	// Without identity: the tool refuses (identity comes only from the header).
	fake.sawUser = ""
	out, isErr = callMyProjects(t, ts.URL, "")
	if !isErr {
		t.Errorf("without identity: expected an error result, got: %s", out)
	}
	if fake.sawUser != "" {
		t.Errorf("resolver should not have been called without identity, saw %q", fake.sawUser)
	}
}
