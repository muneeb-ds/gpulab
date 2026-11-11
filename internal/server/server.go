// Package server is the gpulab control plane: HTTP API, agent endpoint,
// operation runner and reconciler.
package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/muneeb-ds/gpulab/internal/incus"
	"github.com/muneeb-ds/gpulab/internal/store"
)

type Config struct {
	DataDir    string
	AdminToken string
	PublicURL  string // how agents and the UI reach this server
	// AgentBinary is served at /download/gpulab-agent for the install script.
	AgentBinary string

	ImageServer string // simplestreams server for container images
	ImageAlias  string // default image
	SSHPortMin  int
	SSHPortMax  int

	HeartbeatWait     time.Duration // max long-poll
	ReconcileInterval time.Duration
	TaskTimeout       time.Duration
	OpPollInterval    time.Duration
}

func (c *Config) defaults() {
	if c.ImageServer == "" {
		c.ImageServer = "https://images.linuxcontainers.org"
	}
	if c.ImageAlias == "" {
		c.ImageAlias = "ubuntu/24.04/cloud"
	}
	if c.SSHPortMin == 0 {
		c.SSHPortMin, c.SSHPortMax = 22000, 22999
	}
	if c.HeartbeatWait == 0 {
		c.HeartbeatWait = 25 * time.Second
	}
	if c.ReconcileInterval == 0 {
		c.ReconcileInterval = 15 * time.Second
	}
	if c.TaskTimeout == 0 {
		c.TaskTimeout = 10 * time.Minute
	}
	if c.OpPollInterval == 0 {
		c.OpPollInterval = time.Second
	}
}

type Server struct {
	cfg     Config
	st      *store.Store
	cert    tls.Certificate
	certPEM string

	// IncusFor builds a client for a node. Overridable in tests.
	IncusFor func(n store.Node) incus.Client

	mu      sync.Mutex
	waiters map[int64]chan struct{} // node id -> heartbeat long-poll wakeup
	opsWake chan struct{}
	Logf    func(format string, args ...any)
}

func New(cfg Config, st *store.Store) (*Server, error) {
	cfg.defaults()
	s := &Server{cfg: cfg, st: st, waiters: map[int64]chan struct{}{}, opsWake: make(chan struct{}, 1)}
	if err := s.loadOrCreateCert(); err != nil {
		return nil, err
	}
	s.IncusFor = func(n store.Node) incus.Client { return incus.New(n.IncusURL, n.IncusFingerprint, s.cert) }
	return s, nil
}

func (s *Server) logf(f string, args ...any) {
	if s.Logf != nil {
		s.Logf(f, args...)
	} else {
		log.Printf(f, args...)
	}
}

// Start launches the background loops.
func (s *Server) Start(ctx context.Context) {
	s.st.ResetRunningOps(ctx)
	go s.runOps(ctx)
	go s.reconcileLoop(ctx)
}

// loadOrCreateCert loads the client certificate the control plane presents
// to every worker's Incus API, generating it on first start.
func (s *Server) loadOrCreateCert() error {
	crt, key := filepath.Join(s.cfg.DataDir, "client.crt"), filepath.Join(s.cfg.DataDir, "client.key")
	if _, err := os.Stat(crt); os.IsNotExist(err) {
		pk, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			return err
		}
		serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
		tmpl := &x509.Certificate{
			SerialNumber: serial, Subject: pkix.Name{CommonName: "gpulab-control-plane"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &pk.PublicKey, pk)
		if err != nil {
			return err
		}
		kb, _ := x509.MarshalECPrivateKey(pk)
		if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			return err
		}
	}
	cert, err := tls.LoadX509KeyPair(crt, key)
	if err != nil {
		return fmt.Errorf("load client cert: %w", err)
	}
	b, _ := os.ReadFile(crt)
	s.cert, s.certPEM = cert, string(b)
	return nil
}

func (s *Server) kickNode(id int64) {
	s.mu.Lock()
	ch, ok := s.waiters[id]
	s.mu.Unlock()
	if ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) nodeWaiter(id int64) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.waiters[id]
	if !ok {
		ch = make(chan struct{}, 1)
		s.waiters[id] = ch
	}
	return ch
}

func (s *Server) wakeOps() {
	select {
	case s.opsWake <- struct{}{}:
	default:
	}
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Handler returns the full HTTP handler (API, agent endpoint, web UI).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return mux
}

// CreateJoinToken issues a 24h one-time token (used by --demo and tests).
func (s *Server) CreateJoinToken(ctx context.Context) (string, error) {
	tok := randomToken(16)
	return tok, s.st.CreateJoinToken(ctx, tok, 24*3600)
}
