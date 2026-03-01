// Command transport-webtransport is the entry point for the transport.webtransport
// plugin. It serves browser clients over HTTP/2 TLS via a JSON-RPC 2.0 gateway
// and forwards MCP requests to the Orchestra orchestrator over QUIC+mTLS.
//
// An embedded React dashboard is served at the root path.
//
// Usage:
//
//	transport-webtransport \
//	  --orchestrator-addr localhost:9100 \
//	  --listen-addr :4433 \
//	  --certs-dir ~/.orchestra/certs \
//	  --api-key my-secret-key
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/orchestra-mcp/plugin-transport-webtransport/internal"
	"github.com/orchestra-mcp/sdk-go/plugin"
)

func main() {
	orchestratorAddr := flag.String("orchestrator-addr", "localhost:9100", "Address of the orchestrator")
	listenAddr := flag.String("listen-addr", ":4433", "Address to listen for browser client connections")
	certsDir := flag.String("certs-dir", plugin.DefaultCertsDir, "Directory for mTLS certificates")
	apiKey := flag.String("api-key", "", "Static API key for client authentication (empty = no auth)")
	flag.Parse()

	if *orchestratorAddr == "" {
		log.Fatal("--orchestrator-addr is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintf(os.Stderr, "transport.webtransport: received shutdown signal\n")
		cancel()
	}()

	// Resolve the certs directory (expand ~ if present).
	resolvedCertsDir := plugin.ResolveCertsDir(*certsDir)

	// Set up mTLS client configuration for connecting to the orchestrator.
	clientTLS, err := plugin.ClientTLSConfig(resolvedCertsDir, "transport.webtransport-client")
	if err != nil {
		log.Fatalf("client TLS config: %v", err)
	}

	// Connect to the orchestrator over QUIC.
	client, err := plugin.NewOrchestratorClient(ctx, *orchestratorAddr, clientTLS)
	if err != nil {
		log.Fatalf("connect to orchestrator at %s: %v", *orchestratorAddr, err)
	}
	defer client.Close()

	fmt.Fprintf(os.Stderr, "transport.webtransport: connected to orchestrator at %s\n", *orchestratorAddr)

	// Set up TLS server configuration for accepting browser clients.
	// Regular TLS (not mTLS) — browsers don't have Orchestra CA certificates.
	caCert, caKey, err := plugin.EnsureCA(resolvedCertsDir)
	if err != nil {
		log.Fatalf("ensure CA: %v", err)
	}

	serverCert, err := plugin.GenerateCert(resolvedCertsDir, "webtransport-server", caCert, caKey)
	if err != nil {
		log.Fatalf("generate server cert: %v", err)
	}

	serverTLS, err := internal.BuildServerTLS(serverCert)
	if err != nil {
		log.Fatalf("build server TLS: %v", err)
	}

	// Create the gateway and start serving.
	gw := internal.NewGateway(client, *apiKey, internal.DashboardFS)

	fmt.Fprintf(os.Stderr, "transport.webtransport: dashboard available at https://localhost%s\n", *listenAddr)

	if err := gw.ListenAndServe(ctx, *listenAddr, serverTLS); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "transport.webtransport: shutting down\n")
			return
		}
		log.Fatalf("transport.webtransport: %v", err)
	}
}
