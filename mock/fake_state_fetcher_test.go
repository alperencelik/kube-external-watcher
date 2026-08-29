package mock

import (
	"testing"
	"time"
)

// TestTransformFnCanReprogramWithoutDeadlock verifies that a transform
// callback may call back into the fake's Set*/Reset methods (which take the
// write lock) without self-deadlocking — TransformExternalState must not
// hold the lock while invoking the callback.
func TestTransformFnCanReprogramWithoutDeadlock(t *testing.T) {
	f := NewFakeResourceStateFetcher()
	f.SetTransformFn(func(raw any) (any, error) {
		// Reprogram the fake from inside the transform. If
		// TransformExternalState held f.mu across this call, the write lock
		// here would deadlock against the read lock held by the caller.
		f.SetResourceState("some-key", "some-state")
		return raw, nil
	})

	done := make(chan struct{})
	go func() {
		_, _ = f.TransformExternalState("input")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TransformExternalState deadlocked when the callback reprogrammed the fake")
	}
}
