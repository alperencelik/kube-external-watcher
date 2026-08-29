package watcher

import (
	"fmt"

	"github.com/google/go-cmp/cmp"
)

// DeepEqualComparator is the default StateComparator. It uses
// github.com/google/go-cmp/cmp to compare the desired and actual state.
// Users can supply cmp.Options (e.g. cmpopts.IgnoreFields,
// cmpopts.SortSlices) for fine-grained control over comparison.
//
// Both inputs to HasDrifted are expected to be in the same shape —
// the fetcher's TransformExternalState normalizes the external API response
// before the comparator is called.
type DeepEqualComparator struct {
	opts cmp.Options
}

// NewDeepEqualComparator creates a DeepEqualComparator with the given
// cmp.Options applied to every comparison.
func NewDeepEqualComparator(opts ...cmp.Option) *DeepEqualComparator {
	return &DeepEqualComparator{opts: opts}
}

// HasDrifted returns true if the desired and actual state differ per
// cmp.Equal, indicating the external resource has drifted from the
// desired Kubernetes state.
//
// go-cmp panics when it encounters unexported struct fields with no
// applicable option (e.g. cmpopts.IgnoreUnexported) — a common situation
// when comparing third-party SDK types. HasDrifted recovers from that
// panic and returns it as an error so the poll loop can log and continue
// instead of crashing the process.
func (c *DeepEqualComparator) HasDrifted(desired, actual any) (drifted bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			drifted = false
			err = fmt.Errorf("deep-equal comparison panicked (likely unexported fields — "+
				"supply a cmp.Option such as cmpopts.IgnoreUnexported): %v", r)
		}
	}()
	return !cmp.Equal(desired, actual, c.opts...), nil
}

// Diff returns a human-readable diff between the desired and actual state.
// Like HasDrifted, it recovers from go-cmp panics (e.g. on unexported
// fields) and returns a placeholder rather than propagating the panic.
func (c *DeepEqualComparator) Diff(desired, actual any) (diff string) {
	defer func() {
		if r := recover(); r != nil {
			diff = fmt.Sprintf("<diff unavailable: comparison panicked: %v>", r)
		}
	}()
	return cmp.Diff(desired, actual, c.opts...)
}
