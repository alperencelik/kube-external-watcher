package watcher

import "github.com/google/go-cmp/cmp"

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
func (c *DeepEqualComparator) HasDrifted(desired, actual any) (bool, error) {
	return !cmp.Equal(desired, actual, c.opts...), nil
}
