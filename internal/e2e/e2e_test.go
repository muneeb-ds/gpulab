// End-to-end test: real control plane + real agent code, with a simulated
// worker (fake GPUs, fake Incus API).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muneeb-ds/gpulab/internal/agent"
	"github.com/muneeb-ds/gpulab/internal/demo"
	"github.com/muneeb-ds/gpulab/internal/server"
	"github.com/muneeb-ds/gpulab/internal/store"
)

const admin = "test-admin-token"

type env struct {
	t      *testing.T
	url    string
	worker *demo.Worker
}

func (e *env) call(method, path string, body any, out any) int {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

type overview struct {
	Nodes []struct {
		store.Node
		Online bool
	}
	GPUs       []store.GPU       `json:"gpus"`
	Slices     []store.Slice     `json:"slices"`
	Containers []store.Container `json:"containers"`
}

func (e *env) overview() overview {
	var o overview
	e.call("GET", "/api/overview", nil, &o)
	return o
}

func (e *env) waitFor(what string, cond func(o overview) bool) overview {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		o := e.overview()
		if cond(o) {
			return o
		}
		time.Sleep(50 * time.Millisecond)
	}
	o := e.overview()
	var ops []store.Operation
	e.call("GET", "/api/operations", nil, &ops)
	b, _ := json.MarshalIndent(map[string]any{"overview": o, "ops": ops}, "", " ")
	e.t.Fatalf("timed out waiting for %s\n%s", what, b)
	return o
}

func setup(t *testing.T) *env {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := server.New(server.Config{DataDir: dir, AdminToken: admin, HeartbeatWait: 300 * time.Millisecond,
		ReconcileInterval: 200 * time.Millisecond, OpPollInterval: 50 * time.Millisecond, TaskTimeout: 10 * time.Second}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv.Logf = t.Logf
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv.Start(ctx)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	w := demo.NewWorker()
	t.Cleanup(w.Close)
	e := &env{t: t, url: hs.URL, worker: w}

	var tok struct{ Token string }
	if code := e.call("POST", "/api/join-tokens", nil, &tok); code != 200 {
		t.Fatalf("join token: %d", code)
	}
	a := &agent.Agent{Server: hs.URL, JoinToken: tok.Token, StateDir: filepath.Join(dir, "agent"), IncusURL: w.Incus.URL,
		Backend: w.GPUs, Local: demo.LocalIncus{W: w}, PollWait: 300 * time.Millisecond,
		Logf: func(f string, a ...any) { t.Logf("[agent] "+f, a...) }}
	go a.Run(ctx)
	return e
}

func gpuByModel(o overview, sub string) store.GPU {
	for _, g := range o.GPUs {
		if strings.Contains(g.Model, sub) {
			return g
		}
	}
	return store.GPU{}
}

func slicesOf(o overview, gpuID int64) []store.Slice {
	var out []store.Slice
	for _, s := range o.Slices {
		if s.GPUID == gpuID {
			out = append(out, s)
		}
	}
	return out
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)

	// 1. Worker registers and reports two GPUs; approve it.
	o := e.waitFor("node with 2 GPUs", func(o overview) bool { return len(o.Nodes) == 1 && len(o.GPUs) == 2 })
	nodeID := o.Nodes[0].ID
	if o.Nodes[0].Status != "pending" {
		t.Fatalf("new node should be pending, got %s", o.Nodes[0].Status)
	}
	if code := e.call("POST", fmt.Sprintf("/api/nodes/%d/approve", nodeID), nil, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	o = e.waitFor("node trusted", func(o overview) bool { return o.Nodes[0].Trusted })

	a100 := gpuByModel(o, "A100")
	rtx := gpuByModel(o, "4090")
	if !a100.MigCapable || rtx.MigCapable {
		t.Fatalf("unexpected capabilities: %+v %+v", a100, rtx)
	}
	if len(slicesOf(o, rtx.ID)) != 1 || slicesOf(o, rtx.ID)[0].Kind != "full" {
		t.Fatalf("RTX should expose one full-GPU slice: %+v", slicesOf(o, rtx.ID))
	}

	// 2. Invalid layout is rejected; valid one is applied.
	var v struct {
		OK    bool
		Error string
	}
	e.call("POST", fmt.Sprintf("/api/gpus/%d/validate", a100.ID), map[string]any{"mode": "mig", "profiles": []string{"7g.40gb", "1g.5gb"}}, &v)
	if v.OK {
		t.Fatal("7g + 1g should not fit")
	}
	layout := []string{"3g.20gb", "2g.10gb", "1g.5gb", "1g.5gb"}
	if code := e.call("POST", fmt.Sprintf("/api/gpus/%d/layout", a100.ID), map[string]any{"mode": "mig", "profiles": layout}, nil); code != 200 {
		t.Fatalf("set layout: %d", code)
	}
	o = e.waitFor("4 MIG slices", func(o overview) bool {
		n := 0
		for _, s := range slicesOf(o, a100.ID) {
			if s.Kind == "mig" && s.State == "ready" && strings.HasPrefix(s.MigUUID, "MIG-") {
				n++
			}
		}
		return n == 4 && gpuByModel(o, "A100").Status == "ready"
	})

	// 3. Container with two MIG slices.
	var u struct{ ID int64 }
	e.call("POST", "/api/users", map[string]any{"name": "alice", "ssh_keys": "ssh-ed25519 AAAAC3Nza alice@laptop"}, &u)
	var picked []int64
	for _, s := range slicesOf(o, a100.ID) {
		if s.Profile == "1g.5gb" || s.Profile == "2g.10gb" {
			if len(picked) < 2 {
				picked = append(picked, s.ID)
			}
		}
	}
	var created struct {
		ID    int64
		Error string
	}
	if code := e.call("POST", "/api/containers", map[string]any{"name": "alice-box1", "user_id": u.ID, "slice_ids": picked}, &created); code != 200 {
		t.Fatalf("create container: %d %s", code, created.Error)
	}
	o = e.waitFor("container running", func(o overview) bool {
		return len(o.Containers) == 1 && o.Containers[0].State == "running"
	})
	inst, ok := e.worker.Incus.Instance("alice-box1")
	if !ok || inst.Status != "Running" {
		t.Fatalf("instance not running in Incus: %+v", inst)
	}
	if inst.Config["nvidia.runtime"] != "true" || inst.Config["boot.autostart"] != "false" ||
		!strings.Contains(inst.Config["cloud-init.user-data"], "ssh-ed25519 AAAAC3Nza") {
		t.Fatalf("bad config: %+v", inst.Config)
	}
	if inst.Devices["ssh"]["listen"] != "tcp:0.0.0.0:22000" {
		t.Fatalf("ssh proxy device: %+v", inst.Devices["ssh"])
	}
	uuidsBefore := map[string]bool{}
	for _, d := range inst.Devices {
		if d["type"] == "gpu" {
			if d["gputype"] != "mig" || d["pci"] != a100.PCI {
				t.Fatalf("bad gpu device: %+v", d)
			}
			uuidsBefore[d["mig.uuid"]] = true
		}
	}
	if len(uuidsBefore) != 2 {
		t.Fatalf("expected 2 gpu devices, got %+v", inst.Devices)
	}

	// 4. Assigned slices are protected.
	var errResp struct{ Error string }
	if code := e.call("POST", "/api/containers", map[string]any{"name": "bob-box", "user_id": u.ID, "slice_ids": picked[:1]}, &errResp); code == 200 {
		t.Fatal("double assignment must fail")
	}
	if code := e.call("POST", fmt.Sprintf("/api/gpus/%d/layout", a100.ID), map[string]any{"mode": "mig", "profiles": []string{"7g.40gb"}}, &errResp); code == 200 {
		t.Fatal("layout removing assigned slices must fail")
	}
	if code := e.call("POST", fmt.Sprintf("/api/gpus/%d/layout", a100.ID), map[string]any{"mode": "exclusive"}, &errResp); code == 200 {
		t.Fatal("switching to exclusive with assigned slices must fail")
	}

	// 5. Reboot: MIG instances vanish (new UUIDs on recreate), containers stop.
	e.worker.Reboot()
	o = e.waitFor("recovery after reboot", func(o overview) bool {
		in, _ := e.worker.Incus.Instance("alice-box1")
		if in.Status != "Running" {
			return false
		}
		for _, d := range in.Devices {
			if d["type"] == "gpu" && (uuidsBefore[d["mig.uuid"]] || !e.worker.GPUs.HasDevice(d["mig.uuid"], "")) {
				return false
			}
		}
		return len(o.Containers) == 1 && o.Containers[0].State == "running"
	})
	if n := len(slicesOf(o, a100.ID)); n != 4 {
		t.Fatalf("expected the 4 slices to be recreated, got %d", n)
	}

	// 6. Re-validating the current layout keeps assigned slices pinned.
	newLayout := append(layout[:0:0], "3g.20gb", "2g.10gb", "1g.5gb", "1g.5gb")
	e.call("POST", fmt.Sprintf("/api/gpus/%d/validate", a100.ID), map[string]any{"mode": "mig", "profiles": newLayout}, &v)
	if !v.OK {
		t.Fatalf("re-validate same layout: %s", v.Error)
	}

	// 7. Full-GPU container on the RTX, then stop / start / delete.
	rtxSlice := slicesOf(o, rtx.ID)[0].ID
	if code := e.call("POST", "/api/containers", map[string]any{"name": "alice-box2", "user_id": u.ID, "slice_ids": []int64{rtxSlice}}, &created); code != 200 {
		t.Fatalf("create full-GPU container: %s", created.Error)
	}
	e.waitFor("second container running", func(o overview) bool {
		for _, c := range o.Containers {
			if c.Name == "alice-box2" && c.State == "running" {
				return true
			}
		}
		return false
	})
	in2, _ := e.worker.Incus.Instance("alice-box2")
	if in2.Devices["gpu0"]["gputype"] != "physical" || in2.Devices["ssh"]["listen"] != "tcp:0.0.0.0:22001" {
		t.Fatalf("bad devices: %+v", in2.Devices)
	}
	e.call("POST", fmt.Sprintf("/api/containers/%d/stop", created.ID), nil, nil)
	e.waitFor("stopped", func(o overview) bool {
		in, _ := e.worker.Incus.Instance("alice-box2")
		return in.Status == "Stopped"
	})
	e.call("DELETE", fmt.Sprintf("/api/containers/%d", created.ID), nil, nil)
	e.waitFor("deleted and slice freed", func(o overview) bool {
		_, exists := e.worker.Incus.Instance("alice-box2")
		for _, s := range o.Slices {
			if s.ID == rtxSlice && s.ContainerID != 0 {
				return false
			}
		}
		return !exists && len(o.Containers) == 1
	})

	// 8. Swap alice-box1's slices for the 3g slice: restart with new devices.
	var threeG int64
	for _, s := range slicesOf(e.overview(), a100.ID) {
		if s.Profile == "3g.20gb" {
			threeG = s.ID
		}
	}
	box1 := e.overview().Containers[0].ID
	if code := e.call("PUT", fmt.Sprintf("/api/containers/%d/slices", box1), map[string]any{"slice_ids": []int64{threeG}}, &errResp); code != 200 {
		t.Fatalf("swap slices: %s", errResp.Error)
	}
	e.waitFor("box1 on 3g slice", func(o overview) bool {
		in, _ := e.worker.Incus.Instance("alice-box1")
		gpus := 0
		for _, d := range in.Devices {
			if d["type"] == "gpu" {
				gpus++
			}
		}
		return in.Status == "Running" && gpus == 1
	})

	// 9. Now the old slices are free: shrinking the layout is allowed.
	if code := e.call("POST", fmt.Sprintf("/api/gpus/%d/layout", a100.ID), map[string]any{"mode": "mig", "profiles": []string{"3g.20gb", "3g.20gb"}}, &errResp); code != 200 {
		t.Fatalf("shrink layout: %s", errResp.Error)
	}
	e.waitFor("two 3g slices, box1 untouched", func(o overview) bool {
		in, _ := e.worker.Incus.Instance("alice-box1")
		ss := slicesOf(o, a100.ID)
		return len(ss) == 2 && ss[0].Profile == "3g.20gb" && ss[1].Profile == "3g.20gb" && in.Status == "Running" &&
			gpuByModel(o, "A100").Status == "ready"
	})
}
