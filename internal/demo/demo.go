// Package demo wires a simulated worker (fake GPUs + fake Incus) to a
// control plane, for trying the UI and for end-to-end tests.
package demo

import (
	"fmt"

	"encoding/pem"
	"github.com/muneeb-ds/gpulab/internal/agent/gpu"
	"github.com/muneeb-ds/gpulab/internal/incus"
	"github.com/muneeb-ds/gpulab/internal/incus/fakeincus"
)

// Worker is a fake GPU host: its Incus API is a fakeincus server and its GPUs
// are a gpu.Fake.
type Worker struct {
	GPUs  *gpu.Fake
	Incus *fakeincus.Server
}

func NewWorker() *Worker {
	w := &Worker{GPUs: gpu.NewFake(), Incus: fakeincus.New()}
	w.Incus.CheckGPU = func(dev map[string]string) error {
		if !w.GPUs.HasDevice(dev["mig.uuid"], dev["pci"]) {
			return fmt.Errorf("card isn't a NVIDIA GPU or driver isn't properly setup")
		}
		return nil
	}
	return w
}

// Reboot simulates a host reboot: MIG instances vanish, containers stop.
func (w *Worker) Reboot() {
	w.GPUs.Reboot()
	w.Incus.StopAll()
}

func (w *Worker) Close() { w.Incus.Close() }

// LocalIncus implements agent.LocalIncus against the fake server.
type LocalIncus struct{ W *Worker }

func (l LocalIncus) Fingerprint() (string, error) { return l.W.Incus.Fingerprint(), nil }

func (l LocalIncus) TrustCert(_ string, certPEM string) error {
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		return fmt.Errorf("bad PEM")
	}
	l.W.Incus.Trust(incus.Fingerprint(blk.Bytes))
	return nil
}
