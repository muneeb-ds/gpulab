package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/muneeb-ds/gpulab/internal/incus"
	"github.com/muneeb-ds/gpulab/internal/proto"
	"github.com/muneeb-ds/gpulab/internal/store"
)

// runOps executes queued operations. Operations for the same node run one at
// a time, in order; different nodes proceed in parallel.
func (s *Server) runOps(ctx context.Context) {
	t := time.NewTicker(s.cfg.OpPollInterval)
	defer t.Stop()
	for {
		ops, err := s.st.NextOps(ctx)
		if err != nil && ctx.Err() == nil {
			s.logf("ops: %v", err)
		}
		for _, op := range ops {
			if err := s.st.SetOpStatus(ctx, op.ID, "running", ""); err != nil {
				continue
			}
			go func(op store.Operation) {
				err := s.execOp(ctx, op)
				status, msg := "done", ""
				if err != nil {
					status, msg = "failed", err.Error()
					s.logf("op %d %s(%d) failed: %v", op.ID, op.Type, op.TargetID, err)
				}
				s.st.SetOpStatus(context.Background(), op.ID, status, msg)
				s.wakeOps()
			}(op)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.opsWake:
		}
	}
}

func (s *Server) execOp(ctx context.Context, op store.Operation) error {
	switch op.Type {
	case "trust_node":
		return s.opTrustNode(ctx, op.TargetID)
	case "apply_layout":
		return s.opApplyLayout(ctx, op.TargetID)
	case "create_container":
		return s.opCreateContainer(ctx, op.TargetID)
	case "sync_container":
		return s.opSyncContainer(ctx, op.TargetID)
	case "delete_container":
		return s.opDeleteContainer(ctx, op.TargetID)
	}
	return fmt.Errorf("unknown operation %q", op.Type)
}

// runTask hands a task to a node's agent and waits for the result.
func (s *Server) runTask(ctx context.Context, nodeID int64, typ string, payload any) (string, error) {
	id, err := s.st.CreateAgentTask(ctx, nodeID, typ, payload)
	if err != nil {
		return "", err
	}
	s.kickNode(nodeID)
	deadline := time.Now().Add(s.cfg.TaskTimeout)
	for time.Now().Before(deadline) {
		t, err := s.st.GetAgentTask(ctx, id)
		if err != nil {
			return "", err
		}
		switch t.Status {
		case "done":
			return t.Output, nil
		case "failed":
			return "", errors.New(t.Error)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("agent did not complete task %d within %s (is the worker online?)", id, s.cfg.TaskTimeout)
}

func (s *Server) opTrustNode(ctx context.Context, nodeID int64) error {
	if _, err := s.runTask(ctx, nodeID, proto.TaskTrustCert, proto.TrustCertPayload{Name: "gpulab-control-plane", CertPEM: s.certPEM}); err != nil {
		return err
	}
	return s.st.SetNodeTrusted(ctx, nodeID, true)
}

func (s *Server) opApplyLayout(ctx context.Context, gpuID int64) error {
	g, err := s.st.GetGPU(ctx, gpuID)
	if err != nil {
		return err
	}
	slices, err := s.st.GPUSlices(ctx, gpuID)
	if err != nil {
		return err
	}
	if err := checkAssignedKept(g, slices); err != nil {
		s.st.SetGPUStatus(ctx, gpuID, "error", err.Error())
		return err
	}
	s.st.SetGPUStatus(ctx, gpuID, "applying", "")
	out, err := s.runTask(ctx, g.NodeID, proto.TaskApplyLayout, proto.ApplyLayoutPayload{GPUUUID: g.UUID, MIG: g.Mode == "mig", Layout: g.Layout})
	if err != nil {
		s.st.SetGPUStatus(ctx, gpuID, "error", err.Error())
		return err
	}
	var hw proto.GPU
	if err := json.Unmarshal([]byte(out), &hw); err != nil {
		s.st.SetGPUStatus(ctx, gpuID, "error", "bad agent output")
		return err
	}
	g.Status = "ready"
	if err := s.syncSlices(ctx, g, hw); err != nil {
		return err
	}
	return s.st.SetGPUStatus(ctx, gpuID, "ready", "")
}

// checkAssignedKept refuses a layout change that would remove a slice that a
// container is using. Unchanged slices are left alone by the agent, so
// containers on them keep running.
func checkAssignedKept(g store.GPU, slices []store.Slice) error {
	want := map[proto.LayoutEntry]bool{}
	for _, e := range g.Layout {
		want[e] = true
	}
	for _, sl := range slices {
		if sl.ContainerID == 0 {
			continue
		}
		if g.Mode == "exclusive" && sl.Kind == "mig" || g.Mode == "mig" && sl.Kind == "full" {
			return fmt.Errorf("GPU has slices assigned to containers; remove them before switching mode")
		}
		if sl.Kind == "mig" && !want[proto.LayoutEntry{Profile: sl.Profile, Start: sl.PlacementStart}] {
			return fmt.Errorf("slice %s@%d is assigned to container %d", sl.Profile, sl.PlacementStart, sl.ContainerID)
		}
	}
	return nil
}

type ctContext struct {
	ct     store.Container
	node   store.Node
	user   store.User
	slices []store.Slice
	gpus   map[int64]store.GPU
	cli    incus.Client
}

func (s *Server) loadCT(ctx context.Context, id int64) (*ctContext, error) {
	c := &ctContext{gpus: map[int64]store.GPU{}}
	var err error
	if c.ct, err = s.st.GetContainer(ctx, id); err != nil {
		return nil, err
	}
	if c.node, err = s.st.GetNode(ctx, c.ct.NodeID); err != nil {
		return nil, err
	}
	if !c.node.Trusted {
		return nil, fmt.Errorf("node %s is not trusted yet", c.node.Name)
	}
	if c.user, err = s.st.GetUser(ctx, c.ct.UserID); err != nil {
		return nil, err
	}
	if c.slices, err = s.st.ContainerSlices(ctx, id); err != nil {
		return nil, err
	}
	for _, sl := range c.slices {
		if _, ok := c.gpus[sl.GPUID]; !ok {
			g, err := s.st.GetGPU(ctx, sl.GPUID)
			if err != nil {
				return nil, err
			}
			c.gpus[sl.GPUID] = g
		}
	}
	c.cli = s.IncusFor(c.node)
	return c, nil
}

func (c *ctContext) slicesReady() bool {
	for _, sl := range c.slices {
		if sl.State != "ready" {
			return false
		}
	}
	return true
}

func (s *Server) opCreateContainer(ctx context.Context, id int64) error {
	c, err := s.loadCT(ctx, id)
	if err != nil {
		s.st.SetContainerState(ctx, id, "error", err.Error())
		return err
	}
	if _, _, err := c.cli.GetInstance(ctx, c.ct.Name); errors.Is(err, incus.ErrNotFound) {
		if err := c.cli.CreateInstance(ctx, s.instanceSpec(c.ct, c.user, c.slices, c.gpus)); err != nil {
			s.st.SetContainerState(ctx, id, "error", err.Error())
			return err
		}
	} else if err != nil {
		s.st.SetContainerState(ctx, id, "error", err.Error())
		return err
	}
	s.st.SetContainerState(ctx, id, "stopped", "")
	return s.opSyncContainer(ctx, id)
}

// opSyncContainer converges an existing container: GPU devices match its
// assigned slices (with current MIG UUIDs) and power state matches desired.
// MIG devices cannot be hot-plugged, so a device change restarts it.
func (s *Server) opSyncContainer(ctx context.Context, id int64) error {
	c, err := s.loadCT(ctx, id)
	if err != nil {
		return err
	}
	if c.ct.DesiredState == "deleted" {
		return nil
	}
	inst, etag, err := c.cli.GetInstance(ctx, c.ct.Name)
	if err != nil {
		s.st.SetContainerState(ctx, id, "error", err.Error())
		return err
	}
	running := inst.Status == "Running"
	ready := c.slicesReady()

	want := map[string]map[string]string{}
	for name, d := range inst.Devices {
		if d["type"] != "gpu" {
			want[name] = d
		}
	}
	if ready {
		for name, d := range gpuDevices(c.slices, c.gpus) {
			want[name] = d
		}
	}
	devicesChanged := ready && !reflect.DeepEqual(gpuOnly(inst.Devices), gpuOnly(want))

	if running && (c.ct.DesiredState == "stopped" || devicesChanged) {
		if err := c.cli.SetState(ctx, c.ct.Name, "stop", false); err != nil {
			if err := c.cli.SetState(ctx, c.ct.Name, "stop", true); err != nil {
				return err
			}
		}
		running = false
		if inst, etag, err = c.cli.GetInstance(ctx, c.ct.Name); err != nil {
			return err
		}
	}
	if devicesChanged {
		inst.Devices = want
		if len(c.slices) > 0 {
			inst.Config["nvidia.runtime"] = "true"
		}
		if err := c.cli.UpdateInstance(ctx, c.ct.Name, *inst, etag); err != nil {
			s.st.SetContainerState(ctx, id, "error", err.Error())
			return err
		}
	}
	if c.ct.DesiredState == "running" && !running {
		if !ready {
			s.st.SetContainerState(ctx, id, "stopped", "waiting for its GPU slices to be recreated")
			return nil
		}
		if err := c.cli.SetState(ctx, c.ct.Name, "start", false); err != nil {
			s.st.SetContainerState(ctx, id, "error", err.Error())
			return err
		}
		running = true
	}
	state := "stopped"
	if running {
		state = "running"
	}
	return s.st.SetContainerState(ctx, id, state, "")
}

func gpuOnly(d map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for k, v := range d {
		if v["type"] == "gpu" {
			out[k] = v
		}
	}
	return out
}

func (s *Server) opDeleteContainer(ctx context.Context, id int64) error {
	ct, err := s.st.GetContainer(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	node, err := s.st.GetNode(ctx, ct.NodeID)
	if err != nil {
		return err
	}
	cli := s.IncusFor(node)
	if inst, _, err := cli.GetInstance(ctx, ct.Name); err == nil {
		if inst.Status == "Running" {
			if err := cli.SetState(ctx, ct.Name, "stop", true); err != nil {
				return err
			}
		}
		if err := cli.DeleteInstance(ctx, ct.Name); err != nil {
			s.st.SetContainerState(ctx, id, "error", err.Error())
			return err
		}
	} else if !errors.Is(err, incus.ErrNotFound) {
		return err
	}
	return s.st.DeleteContainerRow(ctx, id)
}

// reconcileLoop periodically compares containers with Incus. It catches
// containers that stopped behind our back (e.g. a worker reboot, since
// autostart is off) and starts them again once their slices are ready.
func (s *Server) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.reconcileOnce(ctx)
	}
}

func (s *Server) reconcileOnce(ctx context.Context) {
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if n.Status != "ready" || !n.Trusted {
			continue
		}
		cts, err := s.st.ListContainers(ctx, n.ID)
		if err != nil {
			continue
		}
		cli := s.IncusFor(n)
		for _, ct := range cts {
			if ct.State == "creating" || ct.DesiredState == "deleted" ||
				s.st.HasActiveOp(ctx, "sync_container", ct.ID) || s.st.HasActiveOp(ctx, "create_container", ct.ID) {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			inst, _, err := cli.GetInstance(rctx, ct.Name)
			cancel()
			if err != nil {
				continue // worker unreachable; nothing to do
			}
			running := inst.Status == "Running"
			if running && ct.State != "running" {
				s.st.SetContainerState(ctx, ct.ID, "running", "")
			}
			if !running && ct.State == "running" {
				s.st.SetContainerState(ctx, ct.ID, "stopped", "")
			}
			if running != (ct.DesiredState == "running") {
				slices, _ := s.st.ContainerSlices(ctx, ct.ID)
				ready := true
				for _, sl := range slices {
					ready = ready && sl.State == "ready"
				}
				if ready || running {
					s.st.EnqueueOp(ctx, "sync_container", ct.ID, n.ID, nil)
					s.wakeOps()
				}
			}
		}
	}
}
