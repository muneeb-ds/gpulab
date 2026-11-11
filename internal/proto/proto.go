// Package proto holds the wire types shared by the control plane and the
// worker agent.
package proto

import "encoding/json"

// Placement is a MIG placement in memory-slice units (NVML's
// nvmlGpuInstancePlacement_t).
type Placement struct {
	Start int `json:"start"`
	Size  int `json:"size"`
}

// MigProfile describes one GPU-instance profile a GPU supports.
type MigProfile struct {
	Index         int         `json:"index"` // NVML GPU_INSTANCE_PROFILE_* index
	Name          string      `json:"name"`  // e.g. "1g.10gb"
	ComputeSlices int         `json:"compute_slices"`
	MemoryMB      uint64      `json:"memory_mb"`
	MaxCount      int         `json:"max_count"`
	Placements    []Placement `json:"placements"`
}

// MigInstance is a GPU instance (with its single compute instance) that
// currently exists on a GPU.
type MigInstance struct {
	Profile   string    `json:"profile"`
	GIID      int       `json:"gi_id"`
	CIID      int       `json:"ci_id"`
	Placement Placement `json:"placement"`
	UUID      string    `json:"uuid"` // MIG-xxxx; changes whenever the instance is recreated
	MemoryMB  uint64    `json:"memory_mb"`
}

type GPU struct {
	Index      int    `json:"index"`
	UUID       string `json:"uuid"`
	PCI        string `json:"pci"` // normalised to 0000:41:00.0
	Model      string `json:"model"`
	MemoryMB   uint64 `json:"memory_mb"`
	MigCapable bool   `json:"mig_capable"`
	MigEnabled bool   `json:"mig_enabled"`
	// MigPending is true when MIG mode was changed but needs a GPU reset or
	// reboot to take effect.
	MigPending bool          `json:"mig_pending"`
	Profiles   []MigProfile  `json:"profiles"`
	Instances  []MigInstance `json:"instances"`
}

type Inventory struct {
	Hostname         string `json:"hostname"`
	BootID           string `json:"boot_id"`
	DriverVersion    string `json:"driver_version"`
	CUDAVersion      string `json:"cuda_version"`
	IncusURL         string `json:"incus_url"`
	IncusFingerprint string `json:"incus_fingerprint"`
	CPUCount         int    `json:"cpu_count"`
	MemoryMB         uint64 `json:"memory_mb"`
	GPUs             []GPU  `json:"gpus"`
	Error            string `json:"error,omitempty"` // inventory collection problem
}

type RegisterRequest struct {
	JoinToken string `json:"join_token"`
	Hostname  string `json:"hostname"`
}

type RegisterResponse struct {
	NodeID      int64  `json:"node_id"`
	AgentSecret string `json:"agent_secret"`
}

type HeartbeatRequest struct {
	Inventory Inventory    `json:"inventory"`
	Results   []TaskResult `json:"results"`
}

type HeartbeatResponse struct {
	Tasks []Task `json:"tasks"`
}

const (
	TaskApplyLayout = "apply_layout"
	TaskTrustCert   = "trust_cert"
)

type Task struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type TaskResult struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
}

// LayoutEntry is one MIG instance to exist at an exact placement.
type LayoutEntry struct {
	Profile string `json:"profile"`
	Start   int    `json:"start"`
}

// ApplyLayoutPayload asks the agent to converge a GPU to a layout. Instances
// that already match an entry are left untouched, so containers using them
// keep running.
type ApplyLayoutPayload struct {
	GPUUUID string        `json:"gpu_uuid"`
	MIG     bool          `json:"mig"`
	Layout  []LayoutEntry `json:"layout"`
}

type TrustCertPayload struct {
	Name    string `json:"name"`
	CertPEM string `json:"cert_pem"`
}
