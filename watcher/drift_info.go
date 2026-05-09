package watcher

import (
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// DriftInfo describes a detected drift between the Kubernetes desired
// state and the external resource's actual state. It is returned by
// ExternalWatcher.LastDrift so reconcilers can attach context (timestamp,
// diff) to the reconcile that the watcher just triggered — for example,
// to emit a Kubernetes Event, set a status condition, or fire an alert.
type DriftInfo struct {
	// Key identifies the resource that drifted.
	Key types.NamespacedName

	// DetectedAt is the time at which the watcher observed the drift.
	DetectedAt time.Time

	// Diff is a human-readable summary produced by the StateComparator's
	// Diff method. Format is implementation-defined; callers should not
	// rely on a specific shape.
	Diff string
}
