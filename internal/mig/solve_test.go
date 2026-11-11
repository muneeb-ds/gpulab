package mig

import (
	"testing"

	"github.com/muneeb-ds/gpulab/internal/proto"
)

var a100 = BuiltinProfiles("NVIDIA A100-SXM4-40GB")

func TestSolveValid(t *testing.T) {
	cases := [][]string{
		{"7g.40gb"},
		{"3g.20gb", "3g.20gb"},
		{"3g.20gb", "2g.10gb", "1g.5gb", "1g.5gb"},
		{"1g.5gb", "1g.5gb", "1g.5gb", "1g.5gb", "1g.5gb", "1g.5gb", "1g.5gb"},
		{"4g.20gb", "2g.10gb", "1g.5gb"},
	}
	for _, c := range cases {
		l, err := Solve(a100, c, nil, nil)
		if err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		if len(l) != len(c) {
			t.Fatalf("%v: got %v", c, l)
		}
	}
}

func TestSolveInvalid(t *testing.T) {
	cases := [][]string{
		{"4g.20gb", "4g.20gb"},           // max 1
		{"7g.40gb", "1g.5gb"},            // no room
		{"3g.20gb", "3g.20gb", "1g.5gb"}, // 1g placements 0..6 all covered
		{"9g.99gb"},
	}
	for _, c := range cases {
		if l, err := Solve(a100, c, nil, nil); err == nil {
			t.Fatalf("%v: expected error, got %v", c, l)
		}
	}
}

func TestSolvePinned(t *testing.T) {
	pinned := []proto.LayoutEntry{{Profile: "1g.5gb", Start: 0}}
	l, err := Solve(a100, []string{"1g.5gb", "3g.20gb", "2g.10gb"}, pinned, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range l {
		if e == pinned[0] {
			found = true
		}
	}
	if !found {
		t.Fatalf("pinned entry moved: %v", l)
	}
	// A pinned slice that is not in the wanted profiles is an error.
	if _, err := Solve(a100, []string{"7g.40gb"}, pinned, nil); err == nil {
		t.Fatal("expected error when removing a pinned slice")
	}
}

func TestSolvePrefersExisting(t *testing.T) {
	pref := []proto.LayoutEntry{{Profile: "1g.5gb", Start: 6}}
	l, err := Solve(a100, []string{"1g.5gb", "3g.20gb"}, nil, pref)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l {
		if e.Profile == "1g.5gb" && e.Start != 6 {
			t.Fatalf("existing instance moved: %v", l)
		}
	}
}
