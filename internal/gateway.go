// Package internal implements the HTTP/2 TLS gateway for the
// transport.webtransport plugin. It accepts JSON-RPC 2.0 requests from browser
// clients on POST /rpc, forwards them to the Orchestra orchestrator via
// QUIC+Protobuf, and serves an embedded React dashboard at the root path.
package internal

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/orchestra-mcp/gen-go/orchestra/plugin/v1"
	"github.com/orchestra-mcp/sdk-go/protocol"
)

// Sender abstracts the QUIC client so Gateway can be tested without a real
// network connection.
type Sender interface {
	Send(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error)
}

// Gateway serves browser clients over HTTP/2 TLS. It accepts JSON-RPC 2.0
// requests on POST /rpc and forwards them to the Orchestra orchestrator.
type Gateway struct {
	sender Sender
	apiKey string
	dist   fs.FS
}

// NewGateway creates a new Gateway backed by the given QUIC sender.
// apiKey is the optional static Bearer token for client authentication.
// dist is the embedded dashboard filesystem (DashboardFS from assets.go).
func NewGateway(sender Sender, apiKey string, dist fs.FS) *Gateway {
	return &Gateway{sender: sender, apiKey: apiKey, dist: dist}
}

// BuildServerTLS wraps a tls.Certificate into a *tls.Config suitable for an
// HTTP/2 TLS server. The ALPN protocol list includes "h2" to enable HTTP/2.
func BuildServerTLS(cert tls.Certificate) (*tls.Config, error) {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ListenAndServe starts the HTTP/2 TLS server on addr. It blocks until ctx is
// cancelled or a fatal server error occurs. Graceful shutdown is performed on
// context cancellation.
func (g *Gateway) ListenAndServe(ctx context.Context, addr string, tlsConfig *tls.Config) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", g.handleHealth)
	mux.HandleFunc("POST /rpc", g.handleRPC)
	mux.HandleFunc("OPTIONS /rpc", g.handleCORSPreflight)
	mux.Handle("/", g.serveDashboard())

	srv := &http.Server{
		Addr:         addr,
		Handler:      corsMiddleware(mux),
		TLSConfig:    tlsConfig,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServeTLS("", "")
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// handleHealth responds to GET /health with 200 OK.
func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

// handleCORSPreflight responds to OPTIONS /rpc with 204 + CORS headers.
func (g *Gateway) handleCORSPreflight(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleRPC processes a JSON-RPC 2.0 request and writes the response.
func (g *Gateway) handleRPC(w http.ResponseWriter, r *http.Request) {
	// Validate Content-Type.
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	// Optional API key authentication.
	if g.apiKey != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+g.apiKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Read the request body (max 10 MB).
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		writeJSONRPCError(w, nil, protocol.InternalError, "failed to read request body")
		return
	}

	var req protocol.JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, protocol.ParseError, fmt.Sprintf("parse error: %v", err))
		return
	}

	resp := g.dispatch(r.Context(), &req)
	if resp == nil {
		// Notifications get no response.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// serveDashboard returns an http.Handler that serves the embedded React SPA.
// Any path not found in the embedded FS falls back to index.html so that
// React Router can handle client-side routing.
func (g *Gateway) serveDashboard() http.Handler {
	sub, err := fs.Sub(g.dist, "dist")
	if err != nil {
		// If dist sub-dir is missing just return 404.
		return http.NotFoundHandler()
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to serve the real file first.
		f, err := sub.Open(strings.TrimPrefix(r.URL.Path, "/"))
		if err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		// Fall back to index.html for SPA client-side routing.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

// corsMiddleware wraps a handler to add CORS headers to every response.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")
		next.ServeHTTP(w, r)
	})
}

// writeJSONRPCError writes a JSON-RPC error response to w.
func writeJSONRPCError(w http.ResponseWriter, id any, code int, message string) {
	resp := protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &protocol.JSONRPCError{
			Code:    code,
			Message: message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
