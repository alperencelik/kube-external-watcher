package watcher

import (
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// DriftReason says why the watcher reported drift.
type DriftReason string

const (
	// DriftReasonStateMismatch means the external resource exists but
	// differs from the desired state.
	DriftReasonStateMismatch DriftReason = "StateMismatch"

	// DriftReasonNotFound means the external resource no longer exists.
	DriftReasonNotFound DriftReason = "NotFound"
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

	// Reason says why drift was reported. Reconcilers can use it to
	// handle a removed resource differently from a changed one.
	Reason DriftReason

	// Diff is a human-readable summary produced by the StateComparator's
	// Diff method. Format is implementation-defined; callers should not
	// rely on a specific shape. For DriftReasonNotFound it is a fixed
	// message, as there is no external state to compare.
	Diff string
}
