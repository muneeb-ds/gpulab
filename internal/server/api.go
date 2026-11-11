package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/syedmuneeb/gpulab/internal/mig"
	"github.com/syedmuneeb/gpulab/internal/proto"
	"github.com/syedmuneeb/gpulab/internal/store"
	"github.com/syedmuneeb/gpulab/web"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /agent/v1/register", s.handleRegister)
	mux.HandleFunc("POST /agent/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /install-agent.sh", s.handleInstallScript)
	mux.HandleFunc("GET /download/gpulab-agent", s.handleAgentDownload)

	admin := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.requireAdmin(h)) }
	admin("GET /api/overview", s.handleOverview)
	admin("POST /api/join-tokens", s.handleJoinToken)
	admin("POST /api/nodes/{id}/approve", s.handleApproveNode)
	admin("PATCH /api/nodes/{id}", s.handlePatchNode)
	admin("DELETE /api/nodes/{id}", s.handleDeleteNode)
	admin("POST /api/gpus/{id}/validate", s.handleValidateLayout)
	admin("POST /api/gpus/{id}/layout", s.handleSetLayout)
	admin("POST /api/users", s.handleCreateUser)
	admin("DELETE /api/users/{id}", s.handleDeleteUser)
	admin("POST /api/containers", s.handleCreateContainer)
	admin("POST /api/containers/{id}/start", s.handleContainerPower("running"))
	admin("POST /api/containers/{id}/stop", s.handleContainerPower("stopped"))
	admin("PUT /api/containers/{id}/slices", s.handleContainerSlices)
	admin("DELETE /api/containers/{id}", s.handleDeleteContainer)
	admin("GET /api/operations", s.handleOps)

	mux.Handle("GET /", http.FileServerFS(web.FS))
}

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.cfg.AdminToken == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) != 1 {
			httpErr(w, 401, "admin token required")
			return
		}
		h(w, r)
	}
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpErr(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// ---- overview ----

type nodeView struct {
	store.Node
	Online bool `json:"online"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodes, err1 := s.st.ListNodes(ctx)
	gpus, err2 := s.st.ListGPUs(ctx, 0)
	slices, err3 := s.st.ListSlices(ctx)
	cts, err4 := s.st.ListContainers(ctx, 0)
	users, err5 := s.st.ListUsers(ctx)
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	online := time.Now().Unix() - int64(2*s.cfg.HeartbeatWait/time.Second) - 10
	nv := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		nv = append(nv, nodeView{n, n.LastSeen >= online})
	}
	writeJSON(w, map[string]any{
		"nodes": nv, "gpus": nonNil(gpus), "slices": nonNil(slices), "containers": nonNil(cts), "users": nonNil(users),
		"default_image": s.cfg.ImageAlias,
	})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ---- nodes ----

func (s *Server) handleJoinToken(w http.ResponseWriter, r *http.Request) {
	tok := randomToken(16)
	if err := s.st.CreateJoinToken(r.Context(), tok, 24*3600); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	base := strings.TrimRight(s.cfg.PublicURL, "/")
	writeJSON(w, map[string]string{
		"token":           tok,
		"expires_in":      "24h",
		"install_command": fmt.Sprintf("curl -fsSL %s/install-agent.sh | sudo bash -s -- --server %s --join-token %s", base, base, tok),
	})
}

func (s *Server) handleApproveNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n, err := s.st.GetNode(ctx, pathID(r))
	if err != nil {
		httpErr(w, 404, "node not found")
		return
	}
	if n.IncusFingerprint == "" {
		httpErr(w, 409, "the agent has not reported its Incus server yet: "+n.InventoryError)
		return
	}
	s.st.SetNodeStatus(ctx, n.ID, "ready")
	opID, _ := s.st.EnqueueOp(ctx, "trust_node", n.ID, n.ID, nil)
	s.wakeOps()
	writeJSON(w, map[string]any{"operation_id": opID})
}

func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SSHHost *string `json:"ssh_host"`
		Status  *string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	id := pathID(r)
	if req.SSHHost != nil {
		s.st.SetNodeSSHHost(r.Context(), id, strings.TrimSpace(*req.SSHHost))
	}
	if req.Status != nil && (*req.Status == "ready" || *req.Status == "disabled") {
		s.st.SetNodeStatus(r.Context(), id, *req.Status)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	cts, _ := s.st.ListContainers(r.Context(), pathID(r))
	if len(cts) > 0 {
		httpErr(w, 409, "delete the node's containers first")
		return
	}
	s.st.DeleteNode(r.Context(), pathID(r))
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- GPU layouts ----

type layoutReq struct {
	Mode     string   `json:"mode"`     // exclusive|mig
	Profiles []string `json:"profiles"` // for mig
}

// planLayout validates a request and computes exact placements, keeping
// assigned slices (and, where possible, unassigned ones) where they are.
func (s *Server) planLayout(r *http.Request, g store.GPU, req layoutReq) ([]proto.LayoutEntry, error) {
	slices, err := s.st.GPUSlices(r.Context(), g.ID)
	if err != nil {
		return nil, err
	}
	var pinned, preferred []proto.LayoutEntry
	assigned := 0
	for _, sl := range slices {
		if sl.ContainerID != 0 {
			assigned++
		}
		if sl.Kind != "mig" {
			continue
		}
		e := proto.LayoutEntry{Profile: sl.Profile, Start: sl.PlacementStart}
		if sl.ContainerID != 0 {
			pinned = append(pinned, e)
		} else {
			preferred = append(preferred, e)
		}
	}
	switch req.Mode {
	case "exclusive":
		if g.Mode == "mig" && assigned > 0 {
			return nil, fmt.Errorf("%d slice(s) are assigned to containers; remove them first", assigned)
		}
		return nil, nil
	case "mig":
		if !g.MigCapable {
			return nil, fmt.Errorf("%s does not support MIG", g.Model)
		}
		if g.Mode == "exclusive" && assigned > 0 {
			return nil, fmt.Errorf("the full GPU is assigned to a container; remove it first")
		}
		profiles := g.Profiles
		if len(profiles) == 0 {
			profiles = mig.BuiltinProfiles(g.Model)
		}
		if len(profiles) == 0 {
			return nil, fmt.Errorf("no MIG profile catalog for %s yet; enable MIG on the worker once so the agent can report it", g.Model)
		}
		if len(req.Profiles) == 0 {
			return nil, fmt.Errorf("choose at least one MIG profile")
		}
		return mig.Solve(profiles, req.Profiles, pinned, preferred)
	}
	return nil, fmt.Errorf("mode must be exclusive or mig")
}

func (s *Server) handleValidateLayout(w http.ResponseWriter, r *http.Request) {
	var req layoutReq
	if !decode(w, r, &req) {
		return
	}
	g, err := s.st.GetGPU(r.Context(), pathID(r))
	if err != nil {
		httpErr(w, 404, "gpu not found")
		return
	}
	layout, err := s.planLayout(r, g, req)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "layout": nonNil(layout)})
}

func (s *Server) handleSetLayout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req layoutReq
	if !decode(w, r, &req) {
		return
	}
	g, err := s.st.GetGPU(ctx, pathID(r))
	if err != nil {
		httpErr(w, 404, "gpu not found")
		return
	}
	if g.Status == "applying" || s.st.HasActiveOp(ctx, "apply_layout", g.ID) {
		httpErr(w, 409, "a layout change is already in progress for this GPU")
		return
	}
	n, err := s.st.GetNode(ctx, g.NodeID)
	if err != nil || n.Status != "ready" || !n.Trusted {
		httpErr(w, 409, "approve the node before changing its GPUs")
		return
	}
	layout, err := s.planLayout(r, g, req)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	if !g.MigCapable { // nothing to do on the hardware
		writeJSON(w, map[string]any{"layout": []proto.LayoutEntry{}})
		return
	}
	if err := s.st.SetGPUDesired(ctx, g.ID, req.Mode, layout); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	s.st.SetGPUStatus(ctx, g.ID, "ready", "") // clear an old error so the op can run
	opID, _ := s.st.EnqueueOp(ctx, "apply_layout", g.ID, g.NodeID, nil)
	s.wakeOps()
	writeJSON(w, map[string]any{"operation_id": opID, "layout": nonNil(layout)})
}

// ---- users ----

var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var reservedUsers = map[string]bool{"root": true, "daemon": true, "bin": true, "sys": true, "nobody": true, "ubuntu": true, "admin": true}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		SSHKeys string `json:"ssh_keys"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !userRe.MatchString(req.Name) || reservedUsers[req.Name] {
		httpErr(w, 422, "user name must be a valid, non-reserved Linux user name (lowercase letters, digits, - and _)")
		return
	}
	for _, k := range strings.Split(req.SSHKeys, "\n") {
		k = strings.TrimSpace(k)
		if k != "" && !(strings.HasPrefix(k, "ssh-") || strings.HasPrefix(k, "ecdsa-") || strings.HasPrefix(k, "sk-")) {
			httpErr(w, 422, "each line must be an OpenSSH public key")
			return
		}
	}
	id, err := s.st.CreateUser(r.Context(), req.Name, req.SSHKeys)
	if err != nil {
		httpErr(w, 409, "user already exists")
		return
	}
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	cts, _ := s.st.ListContainers(r.Context(), 0)
	for _, c := range cts {
		if c.UserID == pathID(r) {
			httpErr(w, 409, "the user still owns containers")
			return
		}
	}
	s.st.DeleteUser(r.Context(), pathID(r))
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- containers ----

var ctNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// checkSlices verifies slices exist, are usable, live on one node and are
// free (or already belong to container self).
func (s *Server) checkSlices(r *http.Request, ids []int64, self int64) (nodeID int64, err error) {
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			return 0, fmt.Errorf("slice %d listed twice", id)
		}
		seen[id] = true
		sl, err := s.st.GetSlice(r.Context(), id)
		if err != nil {
			return 0, fmt.Errorf("slice %d not found", id)
		}
		if sl.State != "ready" {
			return 0, fmt.Errorf("slice %d is not ready", id)
		}
		if sl.ContainerID != 0 && sl.ContainerID != self {
			return 0, fmt.Errorf("slice %d is already assigned", id)
		}
		g, err := s.st.GetGPU(r.Context(), sl.GPUID)
		if err != nil {
			return 0, err
		}
		if g.Status == "applying" {
			return 0, fmt.Errorf("GPU of slice %d is being reconfigured", id)
		}
		if nodeID != 0 && g.NodeID != nodeID {
			return 0, fmt.Errorf("all slices of a container must be on the same worker")
		}
		nodeID = g.NodeID
	}
	return nodeID, nil
}

func (s *Server) handleCreateContainer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Name     string  `json:"name"`
		UserID   int64   `json:"user_id"`
		NodeID   int64   `json:"node_id"` // only needed without slices
		Image    string  `json:"image"`
		CPU      int     `json:"cpu"`
		MemoryMB int64   `json:"memory_mb"`
		SliceIDs []int64 `json:"slice_ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !ctNameRe.MatchString(req.Name) {
		httpErr(w, 422, "name must be 2-63 chars: lowercase letters, digits and dashes, starting with a letter")
		return
	}
	if _, err := s.st.GetUser(ctx, req.UserID); err != nil {
		httpErr(w, 422, "user not found")
		return
	}
	nodeID, err := s.checkSlices(r, req.SliceIDs, 0)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	if nodeID == 0 {
		nodeID = req.NodeID
	}
	n, err := s.st.GetNode(ctx, nodeID)
	if err != nil {
		httpErr(w, 422, "pick at least one GPU slice or a node")
		return
	}
	if n.Status != "ready" || !n.Trusted {
		httpErr(w, 409, "node "+n.Name+" is not ready")
		return
	}
	if req.Image == "" {
		req.Image = s.cfg.ImageAlias
	}
	if req.CPU <= 0 {
		req.CPU = 4
	}
	if req.MemoryMB <= 0 {
		req.MemoryMB = 16384
	}
	id, err := s.st.CreateContainer(ctx, store.Container{Name: req.Name, UserID: req.UserID, NodeID: nodeID, Image: req.Image,
		CPULimit: req.CPU, MemoryMB: req.MemoryMB, DesiredState: "running"}, req.SliceIDs, s.cfg.SSHPortMin, s.cfg.SSHPortMax)
	if err != nil {
		code := 409
		if !errors.Is(err, store.ErrSliceTaken) && !errors.Is(err, store.ErrNoPorts) && !strings.Contains(err.Error(), "UNIQUE") {
			code = 500
		}
		httpErr(w, code, err.Error())
		return
	}
	opID, _ := s.st.EnqueueOp(ctx, "create_container", id, nodeID, nil)
	s.wakeOps()
	writeJSON(w, map[string]any{"id": id, "operation_id": opID})
}

func (s *Server) handleContainerPower(desired string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ct, err := s.st.GetContainer(r.Context(), pathID(r))
		if err != nil || ct.DesiredState == "deleted" {
			httpErr(w, 404, "container not found")
			return
		}
		s.st.SetContainerDesired(r.Context(), ct.ID, desired)
		opID, _ := s.st.EnqueueOp(r.Context(), "sync_container", ct.ID, ct.NodeID, nil)
		s.wakeOps()
		writeJSON(w, map[string]any{"operation_id": opID})
	}
}

func (s *Server) handleContainerSlices(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SliceIDs []int64 `json:"slice_ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	ct, err := s.st.GetContainer(r.Context(), pathID(r))
	if err != nil {
		httpErr(w, 404, "container not found")
		return
	}
	nodeID, err := s.checkSlices(r, req.SliceIDs, ct.ID)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	if nodeID != 0 && nodeID != ct.NodeID {
		httpErr(w, 422, "slices must be on the container's worker")
		return
	}
	if err := s.st.ReplaceAssignments(r.Context(), ct.ID, req.SliceIDs); err != nil {
		httpErr(w, 409, err.Error())
		return
	}
	opID, _ := s.st.EnqueueOp(r.Context(), "sync_container", ct.ID, ct.NodeID, nil)
	s.wakeOps()
	writeJSON(w, map[string]any{"operation_id": opID})
}

func (s *Server) handleDeleteContainer(w http.ResponseWriter, r *http.Request) {
	ct, err := s.st.GetContainer(r.Context(), pathID(r))
	if err != nil {
		httpErr(w, 404, "container not found")
		return
	}
	s.st.SetContainerDesired(r.Context(), ct.ID, "deleted")
	opID, _ := s.st.EnqueueOp(r.Context(), "delete_container", ct.ID, ct.NodeID, nil)
	s.wakeOps()
	writeJSON(w, map[string]any{"operation_id": opID})
}

func (s *Server) handleOps(w http.ResponseWriter, r *http.Request) {
	ops, err := s.st.ListOps(r.Context(), 50)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, nonNil(ops))
}

// ---- agent install ----

func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	b, err := web.FS.ReadFile("install-agent.sh")
	if err != nil {
		httpErr(w, 404, "missing")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write(b)
}

func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AgentBinary == "" {
		httpErr(w, 404, "this server was started without --agent-binary")
		return
	}
	if _, err := os.Stat(s.cfg.AgentBinary); err != nil {
		httpErr(w, 404, "agent binary not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, s.cfg.AgentBinary)
}
