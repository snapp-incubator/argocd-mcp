// Command argocd-mcp is a read-only ArgoCD MCP server. It exposes ArgoCD
// Applications and AppProjects as summaries for AI agents, scoped to the CALLER:
// every tool authorizes from the caller's identity (X-Remote-User resolved to
// OpenShift groups matched against AppProject roles), so it answers "what can *I*
// access". It never mutates cluster state and never talks to the ArgoCD API — it
// reads the argoproj.io CRs straight from the Kubernetes API with a read-only
// ServiceAccount.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/snapp-incubator/argocd-mcp/internal/k8s"
	"github.com/snapp-incubator/argocd-mcp/internal/mcp"
	"github.com/snapp-incubator/argocd-mcp/internal/version"
)

func main() {
	mcpMode := flag.Bool("mcp", false, "run as stdio MCP server (for local MCP clients)")
	httpAddr := flag.String("http-addr", ":8080", "HTTP listen address (when not in MCP mode)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := k8s.New(k8s.ConfigFromEnv())
	if err != nil {
		log.Error("build kubernetes client", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := mcp.NewServer(client, log)

	if *mcpMode {
		if err := srv.RunStdio(ctx); err != nil && ctx.Err() == nil {
			log.Error("stdio server", "err", err)
			os.Exit(1)
		}
		return
	}
	runHTTP(ctx, srv, client, log, *httpAddr)
}

func runHTTP(ctx context.Context, srv *mcp.Server, client *k8s.Client, log *slog.Logger, addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// readinessTimeout bounds the API-server reachability probe. Kept generous
	// (and configurable) so a transiently slow API or a client-go rate-limiter
	// wait does not flap the pod out of Service endpoints.
	readinessTimeout := 10 * time.Second
	if v := os.Getenv("READINESS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			readinessTimeout = d
		}
	}
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		// Lightweight API-server reachability probe: a single GET /version,
		// no object listing and no RBAC — just "is the API answering".
		if _, err := client.Clientset.Discovery().RESTClient().Get().AbsPath("/version").DoRaw(checkCtx); err != nil {
			log.Warn("readiness check failed", "err", err)
			http.Error(w, "kubernetes api unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": version.String()})
	})
	// WithIdentity lifts the bot-set X-Remote-User header into the request
	// context so identity-scoped tools can resolve the caller's ArgoCD access.
	mux.Handle("/mcp", mcp.WithIdentity(srv.HTTPHandler()))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	hs := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			log.Error("graceful shutdown failed", "err", err)
			_ = hs.Close()
		}
	}()

	log.Info("HTTP server listening", "addr", addr, "endpoint", "http://"+addr+"/mcp")
	if err := hs.ListenAndServe(); err != nil && ctx.Err() == nil {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "shutting down")
}
