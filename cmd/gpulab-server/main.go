// gpulab-server is the control plane: API, web UI and agent endpoint.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/muneeb-ds/gpulab/internal/agent"
	"github.com/muneeb-ds/gpulab/internal/demo"
	"github.com/muneeb-ds/gpulab/internal/server"
	"github.com/muneeb-ds/gpulab/internal/store"
)

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	dataDir := flag.String("data-dir", "./gpulab-data", "database and certificate directory")
	publicURL := flag.String("public-url", "", "URL agents and browsers use to reach this server (default http://localhost<listen>)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate (serve HTTPS)")
	tlsKey := flag.String("tls-key", "", "TLS key")
	agentBin := flag.String("agent-binary", "", "gpulab-agent binary to offer at /download/gpulab-agent")
	image := flag.String("image", "ubuntu/24.04/cloud", "default container image (must include cloud-init)")
	demoMode := flag.Bool("demo", false, "also run a simulated GPU worker (fake A100 + RTX 4090, fake Incus); uses a throwaway data dir")
	flag.Parse()

	token := os.Getenv("GPULAB_ADMIN_TOKEN")
	if token == "" {
		log.Fatal("set GPULAB_ADMIN_TOKEN to the admin token for the UI/API")
	}
	if *publicURL == "" {
		host := *listen
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		*publicURL = "http://" + host
	}
	if *demoMode {
		// The simulated worker lives in memory, so demo state must not outlive the process.
		d, err := os.MkdirTemp("", "gpulab-demo-")
		if err != nil {
			log.Fatal(err)
		}
		defer os.RemoveAll(d)
		*dataDir = d
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(filepath.Join(*dataDir, "gpulab.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	srv, err := server.New(server.Config{DataDir: *dataDir, AdminToken: token, PublicURL: *publicURL,
		AgentBinary: *agentBin, ImageAlias: *image}, st)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.Start(ctx)

	if *demoMode {
		go runDemoWorker(ctx, srv, *publicURL, *dataDir)
	}

	hs := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Printf("gpulab control plane on %s (UI: %s)", *listen, *publicURL)
	if *tlsCert != "" {
		err = hs.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = hs.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func runDemoWorker(ctx context.Context, srv *server.Server, url, dataDir string) {
	w := demo.NewWorker()
	defer w.Close()
	tok, err := srv.CreateJoinToken(ctx)
	if err != nil {
		log.Printf("demo: %v", err)
		return
	}
	a := &agent.Agent{Server: url, JoinToken: tok, StateDir: filepath.Join(dataDir, "demo-agent"),
		IncusURL: w.Incus.URL, Backend: w.GPUs, Local: demo.LocalIncus{W: w},
		Logf: func(f string, args ...any) { log.Printf("[demo-agent] "+f, args...) }}
	time.Sleep(500 * time.Millisecond) // let the HTTP server start
	log.Printf("demo: simulated worker joining; approve it in the UI")
	a.Run(ctx)
}
