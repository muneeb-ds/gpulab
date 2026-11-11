package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/syedmuneeb/gpulab/internal/mig"
	"github.com/syedmuneeb/gpulab/internal/proto"
	"github.com/syedmuneeb/gpulab/internal/store"
)

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req proto.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "bad request")
		return
	}
	name := sanitizeName(req.Hostname)
	if name == "" {
		name = "worker"
	}
	secret := randomToken(32)
	id, err := s.st.ConsumeJoinToken(r.Context(), req.JoinToken, name, hashSecret(secret))
	if errors.Is(err, store.ErrNotFound) {
		httpErr(w, 403, "join token is invalid, expired or already used")
		return
	} else if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	s.logf("node %d (%s) registered; waiting for approval", id, name)
	writeJSON(w, proto.RegisterResponse{NodeID: id, AgentSecret: secret})
}

func (s *Server) agentNode(r *http.Request) (store.Node, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return store.Node{}, false
	}
	n, err := s.st.GetNodeBySecretHash(r.Context(), hashSecret(tok))
	return n, err == nil
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	node, ok := s.agentNode(r)
	if !ok {
		httpErr(w, 401, "unknown agent")
		return
	}
	var req proto.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "bad request")
		return
	}
	ctx := r.Context()
	for _, res := range req.Results {
		if err := s.st.FinishAgentTask(ctx, node.ID, res); err != nil {
			s.logf("node %d: finish task %d: %v", node.ID, res.ID, err)
		}
	}
	if err := s.syncInventory(ctx, node, req.Inventory); err != nil {
		s.logf("node %d: inventory: %v", node.ID, err)
	}

	wait := s.cfg.HeartbeatWait
	if d, err := time.ParseDuration(r.URL.Query().Get("wait")); err == nil && d < wait {
		wait = d
	}
	waiter := s.nodeWaiter(node.ID)
	deadline := time.After(wait)
	for {
		tasks, err := s.st.ClaimAgentTasks(ctx, node.ID, int64(s.cfg.TaskTimeout/time.Second))
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		if len(tasks) > 0 || len(req.Results) > 0 || node.Status == "disabled" {
			writeJSON(w, proto.HeartbeatResponse{Tasks: tasks})
			return
		}
		select {
		case <-waiter:
		case <-deadline:
			writeJSON(w, proto.HeartbeatResponse{Tasks: []proto.Task{}})
			return
		case <-ctx.Done():
			return
		}
	}
}

// syncInventory records what a worker reported and reacts to drift.
func (s *Server) syncInventory(ctx context.Context, node store.Node, inv proto.Inventory) error {
	if node.BootID != "" && inv.BootID != "" && node.BootID != inv.BootID {
		s.logf("node %d (%s) rebooted", node.ID, node.Name)
	}
	if err := s.st.UpdateNodeFacts(ctx, node.ID, inv); err != nil {
		return err
	}
	if inv.Error != "" && len(inv.GPUs) == 0 {
		return nil // don't mark GPUs missing because NVML had a bad moment
	}
	var present []string
	for _, g := range inv.GPUs {
		present = append(present, g.UUID)
		profiles := g.Profiles
		if len(profiles) == 0 && g.MigCapable {
			profiles = mig.BuiltinProfiles(g.Model)
		}
		id, err := s.st.UpsertGPU(ctx, node.ID, g, profiles)
		if err != nil {
			return err
		}
		row, err := s.st.GetGPU(ctx, id)
		if err != nil {
			return err
		}
		if row.Status == "applying" {
			continue // the apply operation will sync slices from its result
		}
		if err := s.syncSlices(ctx, row, g); err != nil {
			return err
		}
		s.checkDrift(ctx, node, row, g)
	}
	return s.st.MarkMissingGPUs(ctx, node.ID, present)
}

func (s *Server) syncSlices(ctx context.Context, row store.GPU, g proto.GPU) error {
	full := row.Mode == "exclusive" && !g.MigEnabled
	changed, err := s.st.SyncSlices(ctx, row.ID, g, full)
	if err != nil {
		return err
	}
	for _, sid := range changed {
		sl, err := s.st.GetSlice(ctx, sid)
		if err == nil && sl.ContainerID != 0 {
			s.st.EnqueueOp(ctx, "sync_container", sl.ContainerID, row.NodeID, nil)
			s.wakeOps()
		}
	}
	return nil
}

// checkDrift re-applies the desired MIG layout when the GPU came back empty
// (MIG instances do not survive a reboot). A GPU with some other unexpected
// layout is left alone and shown to the admin rather than wiped.
func (s *Server) checkDrift(ctx context.Context, node store.Node, row store.GPU, g proto.GPU) {
	if node.Status != "ready" || !node.Trusted || row.Mode != "mig" || row.Status == "error" || len(row.Layout) == 0 {
		return
	}
	if g.MigEnabled && sameLayout(row.Layout, g.Instances) {
		return
	}
	if len(g.Instances) > 0 || s.st.HasActiveOp(ctx, "apply_layout", row.ID) {
		return
	}
	s.logf("gpu %d on %s has no MIG instances; re-applying desired layout", row.ID, node.Name)
	s.st.EnqueueOp(ctx, "apply_layout", row.ID, node.ID, nil)
	s.wakeOps()
}

func sameLayout(want []proto.LayoutEntry, have []proto.MigInstance) bool {
	if len(want) != len(have) {
		return false
	}
	set := map[proto.LayoutEntry]int{}
	for _, e := range want {
		set[e]++
	}
	for _, in := range have {
		k := proto.LayoutEntry{Profile: in.Profile, Start: in.Placement.Start}
		if set[k] == 0 || in.UUID == "" {
			return false
		}
		set[k]--
	}
	return true
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b.WriteRune(c)
		case c == '.' || c == '_':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
