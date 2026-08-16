package mcp

import (
	"context"
	"net/http"
)

// identityHeader carries the authenticated caller's identity (email / SSO
// username), set by the trusted upstream (the SnappCloud bot) — never by the
// model. Identity-scoped tools resolve the caller's ArgoCD access from it.
const identityHeader = "X-Remote-User"

type userKey struct{}

// WithIdentity is HTTP middleware that lifts the X-Remote-User header into the
// request context so tool handlers can read it. The go-sdk streamable handler
// propagates the request context to tool calls, so this is the seam for
// per-request identity.
func WithIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get(identityHeader); u != "" {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
		}
		next.ServeHTTP(w, r)
	})
}

// userFrom returns the caller identity carried in ctx, or "" if none.
func userFrom(ctx context.Context) string {
	u, _ := ctx.Value(userKey{}).(string)
	return u
}
