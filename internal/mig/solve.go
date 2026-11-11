// Package mig validates MIG layouts against a GPU's profile/placement rules.
package mig

import (
	"fmt"
	"sort"

	"github.com/syedmuneeb/gpulab/internal/proto"
)

func mask(p proto.Placement) uint64 { return ((uint64(1) << p.Size) - 1) << p.Start }

// Solve finds a placement for every profile name in want.
//
// pinned entries must keep exactly their placement (they back slices that are
// assigned to containers). preferred entries (existing, unassigned instances)
// are kept where possible so the agent touches as little as it can. The
// returned layout always contains the pinned entries.
func Solve(profiles []proto.MigProfile, want []string, pinned, preferred []proto.LayoutEntry) ([]proto.LayoutEntry, error) {
	byName := map[string]proto.MigProfile{}
	for _, p := range profiles {
		byName[p.Name] = p
	}
	need := map[string]int{}
	for _, w := range want {
		p, ok := byName[w]
		if !ok {
			return nil, fmt.Errorf("profile %q is not supported by this GPU", w)
		}
		need[w]++
		if need[w] > p.MaxCount {
			return nil, fmt.Errorf("profile %s allows at most %d instance(s)", w, p.MaxCount)
		}
	}

	var used uint64
	var out []proto.LayoutEntry
	take := func(e proto.LayoutEntry, strict bool) error {
		p, ok := byName[e.Profile]
		if !ok || need[e.Profile] == 0 {
			if strict {
				return fmt.Errorf("slice %s@%d is assigned to a container and must stay in the layout", e.Profile, e.Start)
			}
			return errSkip
		}
		pl, ok := placementAt(p, e.Start)
		if !ok {
			return fmt.Errorf("profile %s cannot be placed at %d", e.Profile, e.Start)
		}
		if used&mask(pl) != 0 {
			if strict {
				return fmt.Errorf("pinned slice %s@%d overlaps another pinned slice", e.Profile, e.Start)
			}
			return errSkip
		}
		used |= mask(pl)
		need[e.Profile]--
		out = append(out, e)
		return nil
	}
	for _, e := range pinned {
		if err := take(e, true); err != nil {
			return nil, err
		}
	}

	// Try keeping preferred instances first; fall back to pinned only.
	snapUsed, snapOut := used, append([]proto.LayoutEntry(nil), out...)
	snapNeed := copyMap(need)
	for _, e := range preferred {
		_ = take(e, false)
	}
	if rest, ok := place(byName, need, used); ok {
		return sortLayout(append(out, rest...)), nil
	}
	used, out, need = snapUsed, snapOut, snapNeed
	if rest, ok := place(byName, need, used); ok {
		return sortLayout(append(out, rest...)), nil
	}
	return nil, fmt.Errorf("these profiles do not fit on the GPU together")
}

var errSkip = fmt.Errorf("skip")

func copyMap(m map[string]int) map[string]int {
	c := make(map[string]int, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func placementAt(p proto.MigProfile, start int) (proto.Placement, bool) {
	for _, pl := range p.Placements {
		if pl.Start == start {
			return pl, true
		}
	}
	return proto.Placement{}, false
}

// place backtracks over the remaining profiles, biggest first.
func place(byName map[string]proto.MigProfile, need map[string]int, used uint64) ([]proto.LayoutEntry, bool) {
	var todo []proto.MigProfile
	for name, n := range need {
		for i := 0; i < n; i++ {
			todo = append(todo, byName[name])
		}
	}
	sort.Slice(todo, func(i, j int) bool {
		si, sj := placementSize(todo[i]), placementSize(todo[j])
		if si != sj {
			return si > sj
		}
		return todo[i].Name < todo[j].Name
	})
	res := make([]proto.LayoutEntry, len(todo))
	var rec func(i int, used uint64) bool
	rec = func(i int, used uint64) bool {
		if i == len(todo) {
			return true
		}
		for _, pl := range todo[i].Placements {
			if used&mask(pl) == 0 {
				res[i] = proto.LayoutEntry{Profile: todo[i].Name, Start: pl.Start}
				if rec(i+1, used|mask(pl)) {
					return true
				}
			}
		}
		return false
	}
	return res, rec(0, used)
}

func placementSize(p proto.MigProfile) int {
	if len(p.Placements) == 0 {
		return 0
	}
	return p.Placements[0].Size
}

func sortLayout(l []proto.LayoutEntry) []proto.LayoutEntry {
	sort.Slice(l, func(i, j int) bool { return l[i].Start < l[j].Start })
	return l
}
