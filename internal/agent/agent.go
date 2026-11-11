// Package agent is the per-worker daemon: it reports GPU inventory to the
// control plane and executes MIG and trust tasks on the host.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/muneeb-ds/gpulab/internal/agent/gpu"
	"github.com/muneeb-ds/gpulab/internal/proto"
)

type Agent struct {
	Server    string // control plane base URL
	JoinToken string
	StateDir  string
	IncusURL  string // how the control plane reaches this host's Incus API
	Backend   gpu.Backend
	Local     LocalIncus
	HTTP      *http.Client
	PollWait  time.Duration // long-poll duration requested from the server
	Logf      func(format string, args ...any)

	state agentState
}

type agentState struct {
	NodeID int64  `json:"node_id"`
	Secret string `json:"secret"`
	Server string `json:"server"`
}

var errUnauthorized = errors.New("control plane rejected the agent secret")

func (a *Agent) logf(f string, args ...any) {
	if a.Logf != nil {
		a.Logf(f, args...)
	} else {
		log.Printf(f, args...)
	}
}

func (a *Agent) statePath() string { return filepath.Join(a.StateDir, "agent.json") }

func (a *Agent) loadState() {
	b, err := os.ReadFile(a.statePath())
	if err == nil {
		json.Unmarshal(b, &a.state)
	}
	if a.state.Server != "" && a.state.Server != a.Server {
		a.state = agentState{} // registered with a different control plane
	}
}

func (a *Agent) saveState() error {
	if err := os.MkdirAll(a.StateDir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(a.state)
	return os.WriteFile(a.statePath(), b, 0o600)
}

// Run registers (if needed) and then heartbeats until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.HTTP == nil {
		a.HTTP = &http.Client{Timeout: 2 * time.Minute}
	}
	if a.PollWait == 0 {
		a.PollWait = 20 * time.Second
	}
	a.loadState()
	backoff := time.Second
	for a.state.Secret == "" {
		if a.JoinToken == "" {
			return fmt.Errorf("agent is not registered: pass --join-token")
		}
		err := a.register(ctx)
		if err == nil {
			break
		}
		a.logf("register: %v (retrying in %s)", err, backoff)
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, time.Minute)
	}
	a.logf("registered as node %d", a.state.NodeID)

	var results []proto.TaskResult
	backoff = time.Second
	for ctx.Err() == nil {
		tasks, err := a.heartbeat(ctx, results)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				return err
			}
			a.logf("heartbeat: %v (retrying in %s)", err, backoff)
			if !sleep(ctx, backoff) {
				break
			}
			backoff = min(backoff*2, 30*time.Second)
			continue // keep results so they are delivered next time
		}
		backoff = time.Second
		results = nil
		for _, t := range tasks {
			results = append(results, a.execute(t))
		}
	}
	return ctx.Err()
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (a *Agent) post(ctx context.Context, path string, in, out any, auth bool) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.Server, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+a.state.Secret)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && auth {
		return errUnauthorized
	}
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (a *Agent) register(ctx context.Context) error {
	host, _ := os.Hostname()
	var resp proto.RegisterResponse
	if err := a.post(ctx, "/agent/v1/register", proto.RegisterRequest{JoinToken: a.JoinToken, Hostname: host}, &resp, false); err != nil {
		return err
	}
	a.state = agentState{NodeID: resp.NodeID, Secret: resp.AgentSecret, Server: a.Server}
	return a.saveState()
}

func (a *Agent) heartbeat(ctx context.Context, results []proto.TaskResult) ([]proto.Task, error) {
	req := proto.HeartbeatRequest{Inventory: a.inventory(), Results: results}
	var resp proto.HeartbeatResponse
	path := "/agent/v1/heartbeat?wait=" + strconv.Itoa(int(a.PollWait/time.Millisecond)) + "ms"
	err := a.post(ctx, path, req, &resp, true)
	return resp.Tasks, err
}

func (a *Agent) inventory() proto.Inventory {
	inv := proto.Inventory{CPUCount: runtime.NumCPU(), MemoryMB: memTotalMB(), IncusURL: a.IncusURL}
	inv.Hostname, _ = os.Hostname()
	if b, ok := a.Backend.(gpu.BootIDer); ok {
		inv.BootID = b.BootID()
	} else if id, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		inv.BootID = strings.TrimSpace(string(id))
	}
	var errs []string
	gpus, drv, cuda, err := a.Backend.Inventory()
	if err != nil {
		errs = append(errs, "gpu: "+err.Error())
	}
	inv.GPUs, inv.DriverVersion, inv.CUDAVersion = gpus, drv, cuda
	if fp, err := a.Local.Fingerprint(); err != nil {
		errs = append(errs, "incus: "+err.Error())
	} else {
		inv.IncusFingerprint = fp
	}
	inv.Error = strings.Join(errs, "; ")
	return inv
}

func (a *Agent) execute(t proto.Task) proto.TaskResult {
	res := proto.TaskResult{ID: t.ID}
	var err error
	switch t.Type {
	case proto.TaskApplyLayout:
		var p proto.ApplyLayoutPayload
		if err = json.Unmarshal(t.Payload, &p); err != nil {
			break
		}
		a.logf("task %d: apply layout on %s (mig=%v, %d instances)", t.ID, p.GPUUUID, p.MIG, len(p.Layout))
		if err = a.Backend.ApplyLayout(p); err != nil {
			break
		}
		gpus, _, _, ierr := a.Backend.Inventory()
		if ierr != nil {
			err = ierr
			break
		}
		for _, g := range gpus {
			if g.UUID == p.GPUUUID {
				res.Output, _ = json.Marshal(g)
			}
		}
	case proto.TaskTrustCert:
		var p proto.TrustCertPayload
		if err = json.Unmarshal(t.Payload, &p); err == nil {
			a.logf("task %d: trust control-plane certificate", t.ID)
			err = a.Local.TrustCert(p.Name, p.CertPEM)
		}
	default:
		err = fmt.Errorf("unknown task type %q", t.Type)
	}
	if err != nil {
		a.logf("task %d failed: %v", t.ID, err)
		res.Error = err.Error()
	} else {
		res.OK = true
	}
	return res
}

func memTotalMB() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			return kb / 1024
		}
	}
	return 0
}

// DefaultIncusURL guesses this host's address as seen from the control plane.
func DefaultIncusURL(server string) string {
	host := "127.0.0.1"
	if u, err := url.Parse(server); err == nil {
		port := u.Port()
		if port == "" {
			port = "443"
		}
		if c, err := net.Dial("udp", net.JoinHostPort(u.Hostname(), port)); err == nil {
			host = c.LocalAddr().(*net.UDPAddr).IP.String()
			c.Close()
		}
	}
	return "https://" + net.JoinHostPort(host, "8443")
}
