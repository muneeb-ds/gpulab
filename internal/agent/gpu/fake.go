package gpu

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/syedmuneeb/gpulab/internal/mig"
	"github.com/syedmuneeb/gpulab/internal/proto"
)

// Fake simulates a worker with one A100-40GB (MIG capable) and one RTX 4090.
// Like real hardware, MIG instances get new UUIDs every time they are created
// and disappear on Reboot, while MIG mode itself survives.
type Fake struct {
	mu     sync.Mutex
	gpus   []proto.GPU
	bootID string
	nextGI int
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func uuid() string {
	h := randHex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func NewFake() *Fake {
	f := &Fake{bootID: uuid(), nextGI: 1}
	f.gpus = []proto.GPU{
		{Index: 0, UUID: "GPU-" + uuid(), PCI: "0000:41:00.0", Model: "NVIDIA A100-SXM4-40GB", MemoryMB: 40960, MigCapable: true},
		{Index: 1, UUID: "GPU-" + uuid(), PCI: "0000:81:00.0", Model: "NVIDIA GeForce RTX 4090", MemoryMB: 24564},
	}
	return f
}

func (f *Fake) Close() {}

func (f *Fake) BootID() string { f.mu.Lock(); defer f.mu.Unlock(); return f.bootID }

// Reboot drops all MIG instances and changes the boot ID.
func (f *Fake) Reboot() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootID = uuid()
	for i := range f.gpus {
		f.gpus[i].Instances = nil
	}
}

// HasDevice reports whether a MIG UUID or PCI address exists right now.
func (f *Fake) HasDevice(migUUID, pci string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.gpus {
		if migUUID == "" && g.PCI == pci && !g.MigEnabled {
			return true
		}
		for _, in := range g.Instances {
			if migUUID != "" && in.UUID == migUUID {
				return true
			}
		}
	}
	return false
}

func (f *Fake) Inventory() ([]proto.GPU, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]proto.GPU, len(f.gpus))
	for i, g := range f.gpus {
		if g.MigEnabled {
			g.Profiles = mig.BuiltinProfiles(g.Model)
		}
		g.Instances = append([]proto.MigInstance(nil), g.Instances...)
		out[i] = g
	}
	return out, "560.35.03", "12.6", nil
}

func (f *Fake) ApplyLayout(p proto.ApplyLayoutPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var g *proto.GPU
	for i := range f.gpus {
		if f.gpus[i].UUID == p.GPUUUID {
			g = &f.gpus[i]
		}
	}
	if g == nil {
		return fmt.Errorf("GPU %s not found", p.GPUUUID)
	}
	if !p.MIG {
		g.MigEnabled, g.Instances = false, nil
		return nil
	}
	if !g.MigCapable {
		return fmt.Errorf("GPU does not support MIG")
	}
	g.MigEnabled = true
	profiles := map[string]proto.MigProfile{}
	for _, mp := range mig.BuiltinProfiles(g.Model) {
		profiles[mp.Name] = mp
	}
	type key struct {
		p string
		s int
	}
	want := map[key]bool{}
	for _, e := range p.Layout {
		want[key{e.Profile, e.Start}] = true
	}
	var kept []proto.MigInstance
	var used uint64
	have := map[key]bool{}
	for _, in := range g.Instances {
		k := key{in.Profile, in.Placement.Start}
		if want[k] {
			kept = append(kept, in)
			have[k] = true
			used |= ((1 << in.Placement.Size) - 1) << in.Placement.Start
		}
	}
	for _, e := range p.Layout {
		if have[key{e.Profile, e.Start}] {
			continue
		}
		mp, ok := profiles[e.Profile]
		if !ok {
			return fmt.Errorf("profile %s not supported", e.Profile)
		}
		var pl *proto.Placement
		for i := range mp.Placements {
			if mp.Placements[i].Start == e.Start {
				pl = &mp.Placements[i]
			}
		}
		if pl == nil {
			return fmt.Errorf("bad placement %s@%d", e.Profile, e.Start)
		}
		m := uint64((1<<pl.Size)-1) << pl.Start
		if used&m != 0 {
			return fmt.Errorf("insufficient resources for %s@%d", e.Profile, e.Start)
		}
		used |= m
		kept = append(kept, proto.MigInstance{Profile: e.Profile, GIID: f.nextGI, CIID: 0, Placement: *pl, UUID: "MIG-" + uuid(), MemoryMB: mp.MemoryMB})
		f.nextGI++
	}
	g.Instances = kept
	return nil
}
