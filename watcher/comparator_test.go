package watcher

import (
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestDeepEqualComparator_Equal(t *testing.T) {
	c := NewDeepEqualComparator()
	desired := map[string]string{"status": "running"}
	external := map[string]string{"status": "running"}
	drifted, err := c.HasDrifted(desired, external)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drifted {
		t.Error("expected no drift for equal states")
	}
}

func TestDeepEqualComparator_Different(t *testing.T) {
	c := NewDeepEqualComparator()
	desired := map[string]string{"status": "running"}
	external := map[string]string{"status": "stopped"}
	drifted, err := c.HasDrifted(desired, external)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drifted {
		t.Error("expected drift for different states")
	}
}

func TestDeepEqualComparator_BothNil(t *testing.T) {
	c := NewDeepEqualComparator()
	drifted, err := c.HasDrifted(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drifted {
		t.Error("expected no drift when both states are nil")
	}
}

func TestDeepEqualComparator_OneNil(t *testing.T) {
	c := NewDeepEqualComparator()
	drifted, err := c.HasDrifted("running", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drifted {
		t.Error("expected drift when external state is nil but desired state is not")
	}
	drifted, err = c.HasDrifted(nil, "running")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drifted {
		t.Error("expected drift when desired state is nil but external state is not")
	}
}

func TestDeepEqualComparator_ComplexStruct(t *testing.T) {
	type resourceState struct {
		Status  string
		Version int
		Tags    []string
	}

	c := NewDeepEqualComparator()

	desired := resourceState{Status: "running", Version: 14, Tags: []string{"prod", "us-east"}}
	cloudSame := resourceState{Status: "running", Version: 14, Tags: []string{"prod", "us-east"}}
	cloudDiff := resourceState{Status: "running", Version: 15, Tags: []string{"prod", "us-east"}}

	drifted, err := c.HasDrifted(desired, cloudSame)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drifted {
		t.Error("expected no drift for identical complex structs")
	}
	drifted, err = c.HasDrifted(desired, cloudDiff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drifted {
		t.Error("expected drift when version differs")
	}
}

func TestDeepEqualComparator_WithCmpOptions(t *testing.T) {
	type vmState struct {
		Status   string
		IP       string
		LastSeen int64 // changes every poll, should be ignored
	}

	// Ignore LastSeen so it doesn't trigger false drift.
	c := NewDeepEqualComparator(cmpopts.IgnoreFields(vmState{}, "LastSeen"))

	desired := vmState{Status: "running", IP: "10.0.0.1", LastSeen: 1000}
	external := vmState{Status: "running", IP: "10.0.0.1", LastSeen: 2000}

	drifted, err := c.HasDrifted(desired, external)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drifted {
		t.Error("expected no drift when only ignored field changed")
	}

	// Real drift in a non-ignored field should still be detected.
	driftedState := vmState{Status: "stopped", IP: "10.0.0.1", LastSeen: 2000}
	drifted, err = c.HasDrifted(desired, driftedState)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !drifted {
		t.Error("expected drift when status changed")
	}
}

func TestDeepEqualComparator_SortSlices(t *testing.T) {
	type state struct {
		Tags []string
	}

	// Treat slices as sets — order doesn't matter.
	c := NewDeepEqualComparator(cmpopts.SortSlices(func(a, b string) bool { return a < b }))

	desired := state{Tags: []string{"prod", "us-east"}}
	external := state{Tags: []string{"us-east", "prod"}}

	drifted, err := c.HasDrifted(desired, external)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drifted {
		t.Error("expected no drift when slice order differs but elements are the same")
	}
}
