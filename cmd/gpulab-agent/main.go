// gpulab-agent runs on every GPU worker.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/muneeb-ds/gpulab/internal/agent"
	"github.com/muneeb-ds/gpulab/internal/agent/gpu"
)

func main() {
	server := flag.String("server", "", "control plane URL, e.g. https://gpulab.example.com")
	token := flag.String("join-token", "", "one-time join token (only needed for the first start)")
	stateDir := flag.String("state-dir", "/var/lib/gpulab-agent", "where the agent keeps its credentials")
	socket := flag.String("incus-socket", "/var/lib/incus/unix.socket", "local Incus unix socket")
	incusURL := flag.String("incus-url", "", "URL the control plane uses to reach this host's Incus API (default: https://<this host's IP>:8443)")
	flag.Parse()
	if *server == "" {
		*server = os.Getenv("GPULAB_SERVER")
	}
	if *token == "" {
		*token = os.Getenv("GPULAB_JOIN_TOKEN")
	}
	if *server == "" {
		log.Fatal("--server is required")
	}
	if *incusURL == "" {
		*incusURL = agent.DefaultIncusURL(*server)
	}
	backend, err := gpu.NewNVML()
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &agent.Agent{Server: *server, JoinToken: *token, StateDir: *stateDir, IncusURL: *incusURL,
		Backend: backend, Local: agent.NewUnixIncus(*socket)}
	log.Printf("gpulab-agent starting; control plane %s, Incus advertised at %s", *server, *incusURL)
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
