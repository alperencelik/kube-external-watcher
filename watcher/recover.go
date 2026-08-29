package watcher

import (
	"fmt"
	"runtime/debug"

	"github.com/go-logr/logr"
)

// This is used for to prevent go-cmp to panic when comparing structs with unexported fields.
// It's pretty much a catch mechanism for panic and prints the stack trace to the log. The panic is then returned as an error to the caller.

// recoverPanic logs a panic instead of letting it crash the process. The
// watcher runs user code (fetcher, comparator, extractor) in bare goroutines,
// where an unrecovered panic takes down the whole manager.
func recoverPanic(logger logr.Logger, msg string, onPanic func()) {
	r := recover()
	if r == nil {
		return
	}
	logger.Error(fmt.Errorf("%v", r), msg, "stack", string(debug.Stack()))
	if onPanic != nil {
		onPanic()
	}
}
