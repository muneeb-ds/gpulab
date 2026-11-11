// Package fakeincus is an in-memory Incus API server for tests and demos. It
// implements just the endpoints gpulab uses, with async operations, ETags and
// client-certificate trust like the real thing.
package fakeincus

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/syedmuneeb/gpulab/internal/incus"
)

type Server struct {
	*httptest.Server
	mu        sync.Mutex
	instances map[string]*incus.Instance
	versions  map[string]int
	trusted   map[string]bool
	ops       map[string]opResult
	nextOp    int
	// CheckGPU validates a gpu device when an instance starts, like the real
	// server checking that a MIG UUID exists. Optional.
	CheckGPU func(dev map[string]string) error
}

type opResult struct {
	code int
	err  string
}

func New() *Server {
	s := &Server{instances: map[string]*incus.Instance{}, versions: map[string]int{}, trusted: map[string]bool{}, ops: map[string]opResult{}}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	ts.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	ts.StartTLS()
	s.Server = ts
	return s
}

// Fingerprint is the server certificate fingerprint to pin.
func (s *Server) Fingerprint() string { return incus.Fingerprint(s.Certificate().Raw) }

// Trust adds a client certificate (by fingerprint), like `incus config trust add`.
func (s *Server) Trust(fp string) { s.mu.Lock(); s.trusted[fp] = true; s.mu.Unlock() }

// StopAll simulates a host reboot with boot.autostart=false.
func (s *Server) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range s.instances {
		in.Status = "Stopped"
	}
}

func (s *Server) Instance(name string) (incus.Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok {
		return incus.Instance{}, false
	}
	return *in, true
}

func (s *Server) reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) sync(w http.ResponseWriter, meta any) {
	s.reply(w, 200, map[string]any{"type": "sync", "status_code": 200, "metadata": meta})
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	s.reply(w, code, map[string]any{"type": "error", "error_code": code, "error": msg})
}

// async records an already-finished operation (err == "" means success).
func (s *Server) async(w http.ResponseWriter, err string) {
	s.nextOp++
	id := fmt.Sprintf("op-%d", s.nextOp)
	if err == "" {
		s.ops[id] = opResult{code: 200}
	} else {
		s.ops[id] = opResult{code: 400, err: err}
	}
	s.reply(w, 202, map[string]any{"type": "async", "status_code": 100, "operation": "/1.0/operations/" + id})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(r.TLS.PeerCertificates) == 0 || !s.trusted[incus.Fingerprint(r.TLS.PeerCertificates[0].Raw)] {
		s.fail(w, 403, "not authorized")
		return
	}
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/1.0/operations/") && strings.HasSuffix(path, "/wait"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/1.0/operations/"), "/wait")
		op, ok := s.ops[id]
		if !ok {
			s.fail(w, 404, "operation not found")
			return
		}
		status := "Success"
		if op.code != 200 {
			status = "Failure"
		}
		s.sync(w, map[string]any{"status_code": op.code, "status": status, "err": op.err})
	case path == "/1.0/instances" && r.Method == "POST":
		var req incus.InstancesPost
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.fail(w, 400, err.Error())
			return
		}
		if _, ok := s.instances[req.Name]; ok {
			s.fail(w, 409, "instance already exists")
			return
		}
		in := req.Instance
		in.Status = "Stopped"
		in.Architecture = "x86_64"
		s.instances[in.Name] = &in
		s.versions[in.Name] = 1
		s.async(w, "")
	case strings.HasPrefix(path, "/1.0/instances/"):
		rest := strings.TrimPrefix(path, "/1.0/instances/")
		name, sub, _ := strings.Cut(rest, "/")
		in, ok := s.instances[name]
		if !ok {
			s.fail(w, 404, "instance not found")
			return
		}
		etag := fmt.Sprintf("\"%s-%d\"", name, s.versions[name])
		switch {
		case sub == "" && r.Method == "GET":
			w.Header().Set("ETag", etag)
			s.sync(w, in)
		case sub == "" && r.Method == "PUT":
			if m := r.Header.Get("If-Match"); m != "" && m != etag {
				s.fail(w, 412, "ETag doesn't match")
				return
			}
			var put incus.Instance
			json.NewDecoder(r.Body).Decode(&put)
			if in.Status == "Running" && gpuChanged(in.Devices, put.Devices) {
				s.async(w, "MIG devices can't be hotplugged")
				return
			}
			in.Config, in.Devices, in.Profiles, in.Description = put.Config, put.Devices, put.Profiles, put.Description
			s.versions[name]++
			s.async(w, "")
		case sub == "" && r.Method == "DELETE":
			if in.Status == "Running" {
				s.async(w, "instance is running")
				return
			}
			delete(s.instances, name)
			s.async(w, "")
		case sub == "state" && r.Method == "PUT":
			var st struct{ Action string }
			json.NewDecoder(r.Body).Decode(&st)
			switch st.Action {
			case "start":
				if s.CheckGPU != nil {
					for dn, d := range in.Devices {
						if d["type"] == "gpu" {
							if err := s.CheckGPU(d); err != nil {
								s.async(w, fmt.Sprintf("Failed to start device %q: %v", dn, err))
								return
							}
						}
					}
				}
				in.Status = "Running"
			case "stop":
				in.Status = "Stopped"
			}
			s.async(w, "")
		default:
			s.fail(w, 404, "not found")
		}
	default:
		s.fail(w, 404, "not found")
	}
}

func gpuChanged(a, b map[string]map[string]string) bool {
	norm := func(m map[string]map[string]string) string {
		g := map[string]map[string]string{}
		for k, v := range m {
			if v["type"] == "gpu" {
				g[k] = v
			}
		}
		j, _ := json.Marshal(g)
		return string(j)
	}
	return norm(a) != norm(b)
}
