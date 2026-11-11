package gpu

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/muneeb-ds/gpulab/internal/proto"
)

// NVML talks to the driver through libnvidia-ml (loaded at runtime, so the
// binary builds and starts on machines without a GPU).
type NVML struct{ mu sync.Mutex }

func NewNVML() (*NVML, error) {
	if ret := nvml.Init(); ret != nvml.SUCCESS {
		return nil, fmt.Errorf("NVML init failed: %v (is the NVIDIA driver installed?)", ret)
	}
	return &NVML{}, nil
}

func (n *NVML) Close() { nvml.Shutdown() }

func ok(ret nvml.Return) bool { return ret == nvml.SUCCESS }

func cstr[T int8 | uint8](b []T) string {
	var sb strings.Builder
	for _, c := range b {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}

// normPCI turns NVML's "00000000:41:00.0" into Incus's "0000:41:00.0".
func normPCI(s string) string {
	s = strings.ToLower(s)
	if dom, rest, found := strings.Cut(s, ":"); found && len(dom) > 4 {
		s = dom[len(dom)-4:] + ":" + rest
	}
	return s
}

func (n *NVML) Inventory() ([]proto.GPU, string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	driver, _ := nvml.SystemGetDriverVersion()
	cuda := ""
	if v, ret := nvml.SystemGetCudaDriverVersion(); ok(ret) {
		cuda = fmt.Sprintf("%d.%d", v/1000, (v%1000)/10)
	}
	count, ret := nvml.DeviceGetCount()
	if !ok(ret) {
		return nil, driver, cuda, fmt.Errorf("DeviceGetCount: %v", ret)
	}
	var out []proto.GPU
	for i := 0; i < count; i++ {
		d, ret := nvml.DeviceGetHandleByIndex(i)
		if !ok(ret) {
			return nil, driver, cuda, fmt.Errorf("GPU %d: %v", i, ret)
		}
		g := proto.GPU{Index: i}
		g.UUID, _ = d.GetUUID()
		g.Model, _ = d.GetName()
		if pci, ret := d.GetPciInfo(); ok(ret) {
			g.PCI = normPCI(cstr(pci.BusId[:]))
		}
		if mem, ret := d.GetMemoryInfo(); ok(ret) {
			g.MemoryMB = mem.Total / (1 << 20)
		}
		if cur, pend, ret := d.GetMigMode(); ok(ret) {
			g.MigCapable = true
			g.MigEnabled = cur == nvml.DEVICE_MIG_ENABLE
			g.MigPending = cur != pend
			if g.MigEnabled {
				g.Profiles = profiles(d)
				g.Instances = instances(d)
			}
		}
		out = append(out, g)
	}
	return out, driver, cuda, nil
}

func profileName(d nvml.Device, idx int, info nvml.GpuInstanceProfileInfo) string {
	if v3, ret := d.GetGpuInstanceProfileInfoV(idx).V3(); ok(ret) {
		if s := strings.TrimPrefix(cstr(v3.Name[:]), "MIG "); s != "" {
			return s
		}
	}
	return fmt.Sprintf("%dg.%dgb", info.SliceCount, int(math.Round(float64(info.MemorySizeMB)/1024)))
}

func profiles(d nvml.Device) []proto.MigProfile {
	var out []proto.MigProfile
	for p := 0; p < nvml.GPU_INSTANCE_PROFILE_COUNT; p++ {
		info, ret := d.GetGpuInstanceProfileInfo(p)
		if !ok(ret) {
			continue
		}
		mp := proto.MigProfile{Index: p, Name: profileName(d, p, info), ComputeSlices: int(info.SliceCount),
			MemoryMB: info.MemorySizeMB, MaxCount: int(info.InstanceCount)}
		if pls, ret := d.GetGpuInstancePossiblePlacements(&info); ok(ret) {
			for _, pl := range pls {
				mp.Placements = append(mp.Placements, proto.Placement{Start: int(pl.Start), Size: int(pl.Size)})
			}
		}
		out = append(out, mp)
	}
	return out
}

type liveGI struct {
	gi      nvml.GpuInstance
	profile proto.MigProfile
	info    nvml.GpuInstanceInfo
	ciID    int
}

func listGIs(d nvml.Device) []liveGI {
	var out []liveGI
	for _, p := range profiles(d) {
		info, ret := d.GetGpuInstanceProfileInfo(p.Index)
		if !ok(ret) {
			continue
		}
		gis, ret := d.GetGpuInstances(&info)
		if !ok(ret) {
			continue
		}
		for _, gi := range gis {
			gInfo, ret := gi.GetInfo()
			if !ok(ret) {
				continue
			}
			l := liveGI{gi: gi, profile: p, info: gInfo, ciID: -1}
			for _, ci := range computeInstances(gi) {
				if cInfo, ret := ci.GetInfo(); ok(ret) {
					l.ciID = int(cInfo.Id)
					break
				}
			}
			out = append(out, l)
		}
	}
	return out
}

func computeInstances(gi nvml.GpuInstance) []nvml.ComputeInstance {
	var out []nvml.ComputeInstance
	for cp := 0; cp < nvml.COMPUTE_INSTANCE_PROFILE_COUNT; cp++ {
		ciInfo, ret := gi.GetComputeInstanceProfileInfo(cp, nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
		if !ok(ret) {
			continue
		}
		cis, ret := gi.GetComputeInstances(&ciInfo)
		if ok(ret) {
			out = append(out, cis...)
		}
	}
	return out
}

func instances(d nvml.Device) []proto.MigInstance {
	uuids := map[[2]int]string{}
	if max, ret := d.GetMaxMigDeviceCount(); ok(ret) {
		for i := 0; i < max; i++ {
			m, ret := d.GetMigDeviceHandleByIndex(i)
			if !ok(ret) {
				continue
			}
			gi, _ := m.GetGpuInstanceId()
			ci, _ := m.GetComputeInstanceId()
			uuids[[2]int{gi, ci}], _ = m.GetUUID()
		}
	}
	var out []proto.MigInstance
	for _, l := range listGIs(d) {
		out = append(out, proto.MigInstance{
			Profile:   l.profile.Name,
			GIID:      int(l.info.Id),
			CIID:      l.ciID,
			Placement: proto.Placement{Start: int(l.info.Placement.Start), Size: int(l.info.Placement.Size)},
			UUID:      uuids[[2]int{int(l.info.Id), l.ciID}],
			MemoryMB:  l.profile.MemoryMB,
		})
	}
	return out
}

func destroyGI(gi nvml.GpuInstance) error {
	for _, ci := range computeInstances(gi) {
		if ret := ci.Destroy(); !ok(ret) {
			return fmt.Errorf("destroy compute instance: %v (is a process still using it?)", ret)
		}
	}
	if ret := gi.Destroy(); !ok(ret) {
		return fmt.Errorf("destroy GPU instance: %v (is a process still using it?)", ret)
	}
	return nil
}

// createCI creates one compute instance spanning the whole GPU instance.
func createCI(gi nvml.GpuInstance, slices int) error {
	for cp := nvml.COMPUTE_INSTANCE_PROFILE_COUNT - 1; cp >= 0; cp-- {
		ciInfo, ret := gi.GetComputeInstanceProfileInfo(cp, nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
		if ok(ret) && int(ciInfo.SliceCount) == slices {
			if _, ret := gi.CreateComputeInstance(&ciInfo); !ok(ret) {
				return fmt.Errorf("create compute instance: %v", ret)
			}
			return nil
		}
	}
	return fmt.Errorf("no compute-instance profile with %d slices", slices)
}

func (n *NVML) ApplyLayout(p proto.ApplyLayoutPayload) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	d, ret := nvml.DeviceGetHandleByUUID(p.GPUUUID)
	if !ok(ret) {
		return fmt.Errorf("GPU %s: %v", p.GPUUUID, ret)
	}
	cur, _, ret := d.GetMigMode()
	if !ok(ret) {
		if !p.MIG {
			return nil // not MIG capable: exclusive is the only mode
		}
		return fmt.Errorf("GPU does not support MIG: %v", ret)
	}

	if !p.MIG {
		if cur == nvml.DEVICE_MIG_ENABLE {
			for _, l := range listGIs(d) {
				if err := destroyGI(l.gi); err != nil {
					return err
				}
			}
			if _, ret := d.SetMigMode(nvml.DEVICE_MIG_DISABLE); !ok(ret) {
				return fmt.Errorf("disable MIG: %v", ret)
			}
			if c, _, _ := d.GetMigMode(); c != nvml.DEVICE_MIG_DISABLE {
				return fmt.Errorf("MIG disable is pending: reset the GPU (nvidia-smi -r) or reboot the worker")
			}
		}
		return nil
	}

	if cur != nvml.DEVICE_MIG_ENABLE {
		if _, ret := d.SetMigMode(nvml.DEVICE_MIG_ENABLE); !ok(ret) {
			return fmt.Errorf("enable MIG: %v", ret)
		}
		if c, _, _ := d.GetMigMode(); c != nvml.DEVICE_MIG_ENABLE {
			return fmt.Errorf("MIG enable is pending: reset the GPU (nvidia-smi -r) or reboot the worker, then apply again")
		}
	}

	byName := map[string]proto.MigProfile{}
	for _, mp := range profiles(d) {
		byName[mp.Name] = mp
	}
	type key struct {
		profile string
		start   int
	}
	want := map[key]bool{}
	for _, e := range p.Layout {
		if _, ok := byName[e.Profile]; !ok {
			return fmt.Errorf("profile %s is not supported on this GPU", e.Profile)
		}
		want[key{e.Profile, e.Start}] = true
	}
	// Remove instances that are not wanted, repair ones missing a CI.
	have := map[key]bool{}
	for _, l := range listGIs(d) {
		k := key{l.profile.Name, int(l.info.Placement.Start)}
		if !want[k] || have[k] {
			if err := destroyGI(l.gi); err != nil {
				return err
			}
			continue
		}
		have[k] = true
		if l.ciID < 0 {
			if err := createCI(l.gi, l.profile.ComputeSlices); err != nil {
				return err
			}
		}
	}
	// Create what is missing at the exact placement the control plane chose.
	for _, e := range p.Layout {
		k := key{e.Profile, e.Start}
		if have[k] {
			continue
		}
		mp := byName[e.Profile]
		info, ret := d.GetGpuInstanceProfileInfo(mp.Index)
		if !ok(ret) {
			return fmt.Errorf("profile %s: %v", e.Profile, ret)
		}
		size := 0
		for _, pl := range mp.Placements {
			if pl.Start == e.Start {
				size = pl.Size
			}
		}
		if size == 0 {
			return fmt.Errorf("profile %s cannot be placed at %d", e.Profile, e.Start)
		}
		gi, ret := d.CreateGpuInstanceWithPlacement(&info, &nvml.GpuInstancePlacement{Start: uint32(e.Start), Size: uint32(size)})
		if !ok(ret) {
			return fmt.Errorf("create %s at %d: %v", e.Profile, e.Start, ret)
		}
		if err := createCI(gi, mp.ComputeSlices); err != nil {
			return err
		}
		have[k] = true
	}
	return nil
}
