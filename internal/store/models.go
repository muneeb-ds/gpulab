package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/muneeb-ds/gpulab/internal/proto"
)

type User struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	SSHKeys   string `json:"ssh_keys"`
	CreatedAt int64  `json:"created_at"`
}

type Node struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	SecretHash       string `json:"-"`
	Status           string `json:"status"`
	Trusted          bool   `json:"trusted"`
	IncusURL         string `json:"incus_url"`
	IncusFingerprint string `json:"incus_fingerprint"`
	SSHHost          string `json:"ssh_host"`
	BootID           string `json:"boot_id"`
	DriverVersion    string `json:"driver_version"`
	CUDAVersion      string `json:"cuda_version"`
	CPUCount         int    `json:"cpu_count"`
	MemoryMB         int64  `json:"memory_mb"`
	InventoryError   string `json:"inventory_error"`
	LastSeen         int64  `json:"last_seen"`
	CreatedAt        int64  `json:"created_at"`
}

type GPU struct {
	ID         int64               `json:"id"`
	NodeID     int64               `json:"node_id"`
	UUID       string              `json:"uuid"`
	Index      int                 `json:"index"`
	PCI        string              `json:"pci"`
	Model      string              `json:"model"`
	MemoryMB   int64               `json:"memory_mb"`
	MigCapable bool                `json:"mig_capable"`
	MigEnabled bool                `json:"mig_enabled"`
	MigPending bool                `json:"mig_pending"`
	Mode       string              `json:"mode"`
	Layout     []proto.LayoutEntry `json:"layout"`
	Profiles   []proto.MigProfile  `json:"profiles"`
	Status     string              `json:"status"`
	Error      string              `json:"error"`
}

type Slice struct {
	ID             int64  `json:"id"`
	GPUID          int64  `json:"gpu_id"`
	Kind           string `json:"kind"`
	Profile        string `json:"profile"`
	PlacementStart int    `json:"placement_start"`
	GIID           int    `json:"gi_id"`
	CIID           int    `json:"ci_id"`
	MigUUID        string `json:"mig_uuid"`
	MemoryMB       int64  `json:"memory_mb"`
	State          string `json:"state"`
	// Derived from assignments.
	ContainerID int64 `json:"container_id,omitempty"`
}

type Container struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	UserID       int64  `json:"user_id"`
	NodeID       int64  `json:"node_id"`
	Image        string `json:"image"`
	CPULimit     int    `json:"cpu_limit"`
	MemoryMB     int64  `json:"memory_mb"`
	SSHPort      int    `json:"ssh_port"`
	DesiredState string `json:"desired_state"`
	State        string `json:"state"`
	Error        string `json:"error"`
	CreatedAt    int64  `json:"created_at"`
}

type Operation struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"`
	TargetID  int64           `json:"target_id"`
	NodeID    int64           `json:"node_id"`
	Payload   json.RawMessage `json:"payload"`
	Status    string          `json:"status"`
	Error     string          `json:"error"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
}

type AgentTask struct {
	ID     int64
	NodeID int64
	Type   string
	Status string
	Output string
	Error  string
}

// ---- users ----

func (s *Store) CreateUser(ctx context.Context, name, keys string) (int64, error) {
	r, err := s.db.ExecContext(ctx, `INSERT INTO users(name, ssh_keys, created_at) VALUES(?,?,?)`, name, strings.TrimSpace(keys), now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	return err
}

func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT id,name,ssh_keys,created_at FROM users WHERE id=?`, id).Scan(&u.ID, &u.Name, &u.SSHKeys, &u.CreatedAt)
	return u, notFound(err)
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,ssh_keys,created_at FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.SSHKeys, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- join tokens ----

func (s *Store) CreateJoinToken(ctx context.Context, token string, ttlSeconds int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO join_tokens(token, created_at, expires_at) VALUES(?,?,?)`, token, now(), now()+ttlSeconds)
	return err
}

// ConsumeJoinToken creates a pending node if the token is valid and unused.
func (s *Store) ConsumeJoinToken(ctx context.Context, token, nodeName, secretHash string) (int64, error) {
	var id int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var exp int64
		var used sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT expires_at, used_by FROM join_tokens WHERE token=?`, token).Scan(&exp, &used); err != nil {
			return notFound(err)
		}
		if used.Valid || exp < now() {
			return ErrNotFound
		}
		name := nodeName
		for i := 2; ; i++ { // keep node names unique
			var n int
			tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE name=?`, name).Scan(&n)
			if n == 0 {
				break
			}
			name = nodeName + "-" + itoa(i)
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO nodes(name, secret_hash, created_at) VALUES(?,?,?)`, name, secretHash, now())
		if err != nil {
			return err
		}
		id, _ = r.LastInsertId()
		_, err = tx.ExecContext(ctx, `UPDATE join_tokens SET used_by=? WHERE token=?`, id, token)
		return err
	})
	return id, err
}

// ---- nodes ----

const nodeCols = `id,name,secret_hash,status,trusted,incus_url,incus_fingerprint,ssh_host,boot_id,driver_version,cuda_version,cpu_count,memory_mb,inventory_error,last_seen,created_at`

func scanNode(sc interface{ Scan(...any) error }) (Node, error) {
	var n Node
	err := sc.Scan(&n.ID, &n.Name, &n.SecretHash, &n.Status, &n.Trusted, &n.IncusURL, &n.IncusFingerprint, &n.SSHHost, &n.BootID,
		&n.DriverVersion, &n.CUDAVersion, &n.CPUCount, &n.MemoryMB, &n.InventoryError, &n.LastSeen, &n.CreatedAt)
	return n, err
}

func (s *Store) GetNode(ctx context.Context, id int64) (Node, error) {
	n, err := scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id=?`, id))
	return n, notFound(err)
}

func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) UpdateNodeFacts(ctx context.Context, id int64, inv proto.Inventory) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET incus_url=?, incus_fingerprint=?, boot_id=?, driver_version=?, cuda_version=?,
		cpu_count=?, memory_mb=?, inventory_error=?, last_seen=?,
		ssh_host=CASE WHEN ssh_host='' THEN ? ELSE ssh_host END WHERE id=?`,
		inv.IncusURL, inv.IncusFingerprint, inv.BootID, inv.DriverVersion, inv.CUDAVersion, inv.CPUCount, inv.MemoryMB, inv.Error, now(),
		hostOf(inv.IncusURL), id)
	return err
}

func (s *Store) SetNodeStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) SetNodeTrusted(ctx context.Context, id int64, trusted bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET trusted=? WHERE id=?`, trusted, id)
	return err
}

func (s *Store) SetNodeSSHHost(ctx context.Context, id int64, host string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET ssh_host=? WHERE id=?`, host, id)
	return err
}

func (s *Store) DeleteNode(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, id)
	return err
}

// ---- gpus ----

const gpuCols = `id,node_id,uuid,idx,pci,model,memory_mb,mig_capable,mig_enabled,mig_pending,mode,layout,profiles,status,error`

func scanGPU(sc interface{ Scan(...any) error }) (GPU, error) {
	var g GPU
	var layout, profiles string
	err := sc.Scan(&g.ID, &g.NodeID, &g.UUID, &g.Index, &g.PCI, &g.Model, &g.MemoryMB, &g.MigCapable, &g.MigEnabled, &g.MigPending,
		&g.Mode, &layout, &profiles, &g.Status, &g.Error)
	if err == nil {
		json.Unmarshal([]byte(layout), &g.Layout)
		json.Unmarshal([]byte(profiles), &g.Profiles)
	}
	return g, err
}

func (s *Store) GetGPU(ctx context.Context, id int64) (GPU, error) {
	g, err := scanGPU(s.db.QueryRowContext(ctx, `SELECT `+gpuCols+` FROM gpus WHERE id=?`, id))
	return g, notFound(err)
}

func (s *Store) ListGPUs(ctx context.Context, nodeID int64) ([]GPU, error) {
	q, args := `SELECT `+gpuCols+` FROM gpus ORDER BY node_id, idx`, []any{}
	if nodeID != 0 {
		q, args = `SELECT `+gpuCols+` FROM gpus WHERE node_id=? ORDER BY idx`, []any{nodeID}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GPU
	for rows.Next() {
		g, err := scanGPU(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UpsertGPU records what the agent reported. New GPUs start with a desired
// state that matches what is on the hardware right now.
func (s *Store) UpsertGPU(ctx context.Context, nodeID int64, g proto.GPU, profiles []proto.MigProfile) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM gpus WHERE uuid=?`, g.UUID).Scan(&id)
	if err == sql.ErrNoRows {
		mode, layout := "exclusive", []proto.LayoutEntry{}
		if g.MigEnabled {
			mode = "mig"
			for _, in := range g.Instances {
				layout = append(layout, proto.LayoutEntry{Profile: in.Profile, Start: in.Placement.Start})
			}
		}
		r, err := s.db.ExecContext(ctx, `INSERT INTO gpus(node_id,uuid,idx,pci,model,memory_mb,mig_capable,mig_enabled,mig_pending,mode,layout,profiles)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, nodeID, g.UUID, g.Index, g.PCI, g.Model, g.MemoryMB, g.MigCapable, g.MigEnabled, g.MigPending,
			mode, mustJSON(layout), mustJSON(profiles))
		if err != nil {
			return 0, err
		}
		return r.LastInsertId()
	} else if err != nil {
		return 0, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE gpus SET node_id=?, idx=?, pci=?, model=?, memory_mb=?, mig_capable=?, mig_enabled=?, mig_pending=?, profiles=?,
		status=CASE WHEN status='missing' THEN 'ready' ELSE status END WHERE id=?`,
		nodeID, g.Index, g.PCI, g.Model, g.MemoryMB, g.MigCapable, g.MigEnabled, g.MigPending, mustJSON(profiles), id)
	return id, err
}

func (s *Store) MarkMissingGPUs(ctx context.Context, nodeID int64, present []string) error {
	gpus, err := s.ListGPUs(ctx, nodeID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, u := range present {
		seen[u] = true
	}
	for _, g := range gpus {
		if !seen[g.UUID] {
			if _, err := s.db.ExecContext(ctx, `UPDATE gpus SET status='missing' WHERE id=?`, g.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) SetGPUDesired(ctx context.Context, id int64, mode string, layout []proto.LayoutEntry) error {
	if layout == nil {
		layout = []proto.LayoutEntry{}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE gpus SET mode=?, layout=? WHERE id=?`, mode, mustJSON(layout), id)
	return err
}

func (s *Store) SetGPUStatus(ctx context.Context, id int64, status, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE gpus SET status=?, error=? WHERE id=?`, status, msg, id)
	return err
}

// ---- slices ----

const sliceCols = `s.id,s.gpu_id,s.kind,s.profile,s.placement_start,s.gi_id,s.ci_id,s.mig_uuid,s.memory_mb,s.state,COALESCE(a.container_id,0)`

func scanSlice(sc interface{ Scan(...any) error }) (Slice, error) {
	var sl Slice
	err := sc.Scan(&sl.ID, &sl.GPUID, &sl.Kind, &sl.Profile, &sl.PlacementStart, &sl.GIID, &sl.CIID, &sl.MigUUID, &sl.MemoryMB, &sl.State, &sl.ContainerID)
	return sl, err
}

func (s *Store) querySlices(ctx context.Context, where string, args ...any) ([]Slice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sliceCols+` FROM slices s LEFT JOIN assignments a ON a.slice_id=s.id `+where+` ORDER BY s.gpu_id, s.kind, s.placement_start`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Slice
	for rows.Next() {
		sl, err := scanSlice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sl)
	}
	return out, rows.Err()
}

func (s *Store) ListSlices(ctx context.Context) ([]Slice, error) { return s.querySlices(ctx, "") }

func (s *Store) GPUSlices(ctx context.Context, gpuID int64) ([]Slice, error) {
	return s.querySlices(ctx, "WHERE s.gpu_id=?", gpuID)
}

func (s *Store) ContainerSlices(ctx context.Context, containerID int64) ([]Slice, error) {
	return s.querySlices(ctx, "WHERE a.container_id=?", containerID)
}

func (s *Store) GetSlice(ctx context.Context, id int64) (Slice, error) {
	out, err := s.querySlices(ctx, "WHERE s.id=?", id)
	if err != nil {
		return Slice{}, err
	}
	if len(out) == 0 {
		return Slice{}, ErrNotFound
	}
	return out[0], nil
}

// SyncSlices makes the slice rows of a GPU match the hardware. Slices are
// identified by (kind, profile, placement); their MIG UUID is just an
// attribute that is refreshed. Rows that are no longer on the hardware are
// deleted if unassigned and marked missing otherwise. It returns the IDs of
// assigned slices whose UUID changed or that came back after being missing.
func (s *Store) SyncSlices(ctx context.Context, gpuID int64, g proto.GPU, fullGPU bool) (changed []int64, err error) {
	existing, err := s.GPUSlices(ctx, gpuID)
	if err != nil {
		return nil, err
	}
	type key struct {
		kind, profile string
		start         int
	}
	have := map[key]Slice{}
	for _, sl := range existing {
		have[key{sl.Kind, sl.Profile, sl.PlacementStart}] = sl
	}
	seen := map[key]bool{}
	upsert := func(k key, gi, ci int, uuid string, mem uint64) error {
		seen[k] = true
		state := "ready"
		if k.kind == "mig" && uuid == "" {
			state = "missing" // GPU instance without a compute instance: not usable
		}
		if old, ok := have[k]; ok {
			if old.ContainerID != 0 && state == "ready" && (old.MigUUID != uuid || old.State != "ready") {
				changed = append(changed, old.ID)
			}
			_, err := s.db.ExecContext(ctx, `UPDATE slices SET gi_id=?, ci_id=?, mig_uuid=?, memory_mb=?, state=? WHERE id=?`, gi, ci, uuid, mem, state, old.ID)
			return err
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO slices(gpu_id,kind,profile,placement_start,gi_id,ci_id,mig_uuid,memory_mb,state) VALUES(?,?,?,?,?,?,?,?,?)`,
			gpuID, k.kind, k.profile, k.start, gi, ci, uuid, mem, state)
		return err
	}
	if g.MigEnabled {
		for _, in := range g.Instances {
			if err := upsert(key{"mig", in.Profile, in.Placement.Start}, in.GIID, in.CIID, in.UUID, in.MemoryMB); err != nil {
				return nil, err
			}
		}
	} else if fullGPU {
		if err := upsert(key{"full", "", 0}, -1, -1, "", g.MemoryMB); err != nil {
			return nil, err
		}
	}
	for k, sl := range have {
		if seen[k] {
			continue
		}
		if sl.ContainerID == 0 {
			_, err = s.db.ExecContext(ctx, `DELETE FROM slices WHERE id=?`, sl.ID)
		} else {
			_, err = s.db.ExecContext(ctx, `UPDATE slices SET state='missing' WHERE id=?`, sl.ID)
		}
		if err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// ---- containers ----

const ctCols = `id,name,user_id,node_id,image,cpu_limit,memory_mb,ssh_port,desired_state,state,error,created_at`

func scanCT(sc interface{ Scan(...any) error }) (Container, error) {
	var c Container
	err := sc.Scan(&c.ID, &c.Name, &c.UserID, &c.NodeID, &c.Image, &c.CPULimit, &c.MemoryMB, &c.SSHPort, &c.DesiredState, &c.State, &c.Error, &c.CreatedAt)
	return c, err
}

func (s *Store) GetContainer(ctx context.Context, id int64) (Container, error) {
	c, err := scanCT(s.db.QueryRowContext(ctx, `SELECT `+ctCols+` FROM containers WHERE id=?`, id))
	return c, notFound(err)
}

func (s *Store) ListContainers(ctx context.Context, nodeID int64) ([]Container, error) {
	q, args := `SELECT `+ctCols+` FROM containers ORDER BY name`, []any{}
	if nodeID != 0 {
		q, args = `SELECT `+ctCols+` FROM containers WHERE node_id=? ORDER BY name`, []any{nodeID}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Container
	for rows.Next() {
		c, err := scanCT(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateContainer inserts the container, allocates an SSH port on its node and
// assigns the slices, all in one transaction so two requests cannot grab the
// same slice.
func (s *Store) CreateContainer(ctx context.Context, c Container, sliceIDs []int64, portMin, portMax int) (int64, error) {
	var id int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		port, err := freePort(ctx, tx, c.NodeID, portMin, portMax)
		if err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO containers(name,user_id,node_id,image,cpu_limit,memory_mb,ssh_port,desired_state,state,created_at)
			VALUES(?,?,?,?,?,?,?,?,'creating',?)`, c.Name, c.UserID, c.NodeID, c.Image, c.CPULimit, c.MemoryMB, port, c.DesiredState, now())
		if err != nil {
			return err
		}
		id, _ = r.LastInsertId()
		return assign(ctx, tx, id, sliceIDs)
	})
	return id, err
}

func freePort(ctx context.Context, tx *sql.Tx, nodeID int64, min, max int) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT ssh_port FROM containers WHERE node_id=?`, nodeID)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for rows.Next() {
		var p int
		rows.Scan(&p)
		used[p] = true
	}
	rows.Close()
	for p := min; p <= max; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, ErrNoPorts
}

var ErrNoPorts = errorString("no free SSH ports left on this node")

type errorString string

func (e errorString) Error() string { return string(e) }

func assign(ctx context.Context, tx *sql.Tx, containerID int64, sliceIDs []int64) error {
	for i, sid := range sliceIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assignments(slice_id, container_id, device_name) VALUES(?,?,?)`, sid, containerID, "gpu"+itoa(i)); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrSliceTaken
			}
			return err
		}
	}
	return nil
}

var ErrSliceTaken = errorString("a requested slice is already assigned")

// ReplaceAssignments swaps a container's slices atomically.
func (s *Store) ReplaceAssignments(ctx context.Context, containerID int64, sliceIDs []int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE container_id=?`, containerID); err != nil {
			return err
		}
		return assign(ctx, tx, containerID, sliceIDs)
	})
}

func (s *Store) SetContainerState(ctx context.Context, id int64, state, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE containers SET state=?, error=? WHERE id=?`, state, msg, id)
	return err
}

func (s *Store) SetContainerDesired(ctx context.Context, id int64, desired string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE containers SET desired_state=? WHERE id=?`, desired, id)
	return err
}

// DeleteContainerRow removes the container and frees its slices.
func (s *Store) DeleteContainerRow(ctx context.Context, id int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM assignments WHERE container_id=?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM containers WHERE id=?`, id)
		return err
	})
}

// ---- agent tasks ----

func (s *Store) CreateAgentTask(ctx context.Context, nodeID int64, typ string, payload any) (int64, error) {
	r, err := s.db.ExecContext(ctx, `INSERT INTO agent_tasks(node_id,type,payload,created_at,updated_at) VALUES(?,?,?,?,?)`,
		nodeID, typ, mustJSON(payload), now(), now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// ClaimAgentTasks returns queued tasks for a node and marks them sent. Tasks
// that were sent but never answered (agent restarted) are re-sent; agent
// tasks are written to be idempotent.
func (s *Store) ClaimAgentTasks(ctx context.Context, nodeID int64, resendAfter int64) ([]proto.Task, error) {
	var out []proto.Task
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,type,payload FROM agent_tasks WHERE node_id=? AND
			(status='queued' OR (status='sent' AND updated_at < ?)) ORDER BY id`, nodeID, now()-resendAfter)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t proto.Task
			var p string
			rows.Scan(&t.ID, &t.Type, &p)
			t.Payload = json.RawMessage(p)
			out = append(out, t)
		}
		rows.Close()
		for _, t := range out {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='sent', updated_at=? WHERE id=?`, now(), t.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) FinishAgentTask(ctx context.Context, nodeID int64, r proto.TaskResult) error {
	status := "done"
	if !r.OK {
		status = "failed"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE agent_tasks SET status=?, output=?, error=?, updated_at=? WHERE id=? AND node_id=? AND status IN ('queued','sent')`,
		status, string(r.Output), r.Error, now(), r.ID, nodeID)
	return err
}

func (s *Store) GetAgentTask(ctx context.Context, id int64) (AgentTask, error) {
	var t AgentTask
	err := s.db.QueryRowContext(ctx, `SELECT id,node_id,type,status,output,error FROM agent_tasks WHERE id=?`, id).
		Scan(&t.ID, &t.NodeID, &t.Type, &t.Status, &t.Output, &t.Error)
	return t, notFound(err)
}

// ---- operations ----

const opCols = `id,type,target_id,node_id,payload,status,error,created_at,updated_at`

func scanOp(sc interface{ Scan(...any) error }) (Operation, error) {
	var o Operation
	var p string
	err := sc.Scan(&o.ID, &o.Type, &o.TargetID, &o.NodeID, &p, &o.Status, &o.Error, &o.CreatedAt, &o.UpdatedAt)
	o.Payload = json.RawMessage(p)
	return o, err
}

// EnqueueOp adds an operation unless an identical one (same type and target)
// is already queued. It returns the ID of the new or existing operation.
func (s *Store) EnqueueOp(ctx context.Context, typ string, targetID, nodeID int64, payload any) (int64, error) {
	var id int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id FROM operations WHERE type=? AND target_id=? AND status='queued'`, typ, targetID).Scan(&id)
		if err == nil {
			return nil
		}
		if payload == nil {
			payload = struct{}{}
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO operations(type,target_id,node_id,payload,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			typ, targetID, nodeID, mustJSON(payload), now(), now())
		if err != nil {
			return err
		}
		id, err = r.LastInsertId()
		return err
	})
	return id, err
}

func (s *Store) HasActiveOp(ctx context.Context, typ string, targetID int64) bool {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE type=? AND target_id=? AND status IN ('queued','running')`, typ, targetID).Scan(&n)
	return n > 0
}

// NextOps returns queued ops whose node has nothing running, oldest first,
// at most one per node, so operations on one worker run in order.
func (s *Store) NextOps(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+opCols+` FROM operations o WHERE status='queued'
		AND NOT EXISTS (SELECT 1 FROM operations r WHERE r.status='running' AND r.node_id=o.node_id)
		AND id = (SELECT MIN(id) FROM operations q WHERE q.status='queued' AND q.node_id=o.node_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) SetOpStatus(ctx context.Context, id int64, status, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET status=?, error=?, updated_at=? WHERE id=?`, status, msg, now(), id)
	return err
}

// ResetRunningOps requeues ops left running by a crashed control plane.
func (s *Store) ResetRunningOps(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET status='queued' WHERE status='running'`)
	return err
}

func (s *Store) GetOp(ctx context.Context, id int64) (Operation, error) {
	o, err := scanOp(s.db.QueryRowContext(ctx, `SELECT `+opCols+` FROM operations WHERE id=?`, id))
	return o, notFound(err)
}

func (s *Store) ListOps(ctx context.Context, limit int) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+opCols+` FROM operations ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func itoa(i int) string {
	const d = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{d[i%10]}, b...)
	}
	return string(b)
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.LastIndex(u, ":"); i > 0 && !strings.HasSuffix(u, "]") {
		u = u[:i]
	}
	return strings.Trim(u, "[]/")
}

func (s *Store) GetNodeBySecretHash(ctx context.Context, hash string) (Node, error) {
	n, err := scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE secret_hash=?`, hash))
	return n, notFound(err)
}
