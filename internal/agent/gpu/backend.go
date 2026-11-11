// Package gpu discovers NVIDIA GPUs and manages their MIG layout.
package gpu

import "github.com/muneeb-ds/gpulab/internal/proto"

type Backend interface {
	// Inventory reports every GPU with its MIG profiles and live instances.
	Inventory() (gpus []proto.GPU, driver, cuda string, err error)
	// ApplyLayout converges one GPU to the requested MIG state. It must be
	// idempotent: instances already matching an entry are left alone.
	ApplyLayout(p proto.ApplyLayoutPayload) error
	Close()
}

// BootIDer is implemented by backends that simulate a host (the fake).
type BootIDer interface{ BootID() string }
