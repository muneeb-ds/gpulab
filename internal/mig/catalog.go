package mig

import (
	"strings"

	"github.com/syedmuneeb/gpulab/internal/proto"
)

func pl(size int, starts ...int) []proto.Placement {
	out := make([]proto.Placement, len(starts))
	for i, s := range starts {
		out[i] = proto.Placement{Start: s, Size: size}
	}
	return out
}

// Builtin profile catalogs, used to validate layouts for a GPU whose MIG mode
// is still off (NVML only reports profiles once MIG is enabled). Once MIG is
// on, the agent reports the real profiles and those win.
var builtin = []struct {
	match    []string // all substrings must appear in the model name
	profiles []proto.MigProfile
}{
	{[]string{"A100", "40GB"}, []proto.MigProfile{
		{Index: 0, Name: "1g.5gb", ComputeSlices: 1, MemoryMB: 4864, MaxCount: 7, Placements: pl(1, 0, 1, 2, 3, 4, 5, 6)},
		{Index: 7, Name: "1g.10gb", ComputeSlices: 1, MemoryMB: 9856, MaxCount: 4, Placements: pl(2, 0, 2, 4, 6)},
		{Index: 1, Name: "2g.10gb", ComputeSlices: 2, MemoryMB: 9856, MaxCount: 3, Placements: pl(2, 0, 2, 4)},
		{Index: 2, Name: "3g.20gb", ComputeSlices: 3, MemoryMB: 19968, MaxCount: 2, Placements: pl(4, 0, 4)},
		{Index: 3, Name: "4g.20gb", ComputeSlices: 4, MemoryMB: 19968, MaxCount: 1, Placements: pl(4, 0)},
		{Index: 4, Name: "7g.40gb", ComputeSlices: 7, MemoryMB: 40192, MaxCount: 1, Placements: pl(8, 0)},
	}},
	{[]string{"80GB"}, h100Like("10", "20", "40", "80")}, // A100 80GB, H100 80GB
	{[]string{"H100"}, h100Like("10", "20", "40", "80")},
	{[]string{"A30"}, []proto.MigProfile{
		{Index: 0, Name: "1g.6gb", ComputeSlices: 1, MemoryMB: 5836, MaxCount: 4, Placements: pl(1, 0, 1, 2, 3)},
		{Index: 1, Name: "2g.12gb", ComputeSlices: 2, MemoryMB: 11968, MaxCount: 2, Placements: pl(2, 0, 2)},
		{Index: 3, Name: "4g.24gb", ComputeSlices: 4, MemoryMB: 24062, MaxCount: 1, Placements: pl(4, 0)},
	}},
}

func h100Like(g1, g1x2, g3, g7 string) []proto.MigProfile {
	return []proto.MigProfile{
		{Index: 0, Name: "1g." + g1 + "gb", ComputeSlices: 1, MemoryMB: 9856, MaxCount: 7, Placements: pl(1, 0, 1, 2, 3, 4, 5, 6)},
		{Index: 9, Name: "1g." + g1x2 + "gb", ComputeSlices: 1, MemoryMB: 19968, MaxCount: 4, Placements: pl(2, 0, 2, 4, 6)},
		{Index: 1, Name: "2g." + g1x2 + "gb", ComputeSlices: 2, MemoryMB: 19968, MaxCount: 3, Placements: pl(2, 0, 2, 4)},
		{Index: 2, Name: "3g." + g3 + "gb", ComputeSlices: 3, MemoryMB: 40192, MaxCount: 2, Placements: pl(4, 0, 4)},
		{Index: 3, Name: "4g." + g3 + "gb", ComputeSlices: 4, MemoryMB: 40192, MaxCount: 1, Placements: pl(4, 0)},
		{Index: 4, Name: "7g." + g7 + "gb", ComputeSlices: 7, MemoryMB: 80000, MaxCount: 1, Placements: pl(8, 0)},
	}
}

// BuiltinProfiles returns the catalog for a GPU model, or nil if unknown.
func BuiltinProfiles(model string) []proto.MigProfile {
	m := strings.ToUpper(model)
outer:
	for _, b := range builtin {
		for _, s := range b.match {
			if !strings.Contains(m, s) {
				continue outer
			}
		}
		return append([]proto.MigProfile(nil), b.profiles...)
	}
	return nil
}
