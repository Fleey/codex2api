package basispoints

import (
	"fmt"
	"log"
	"sync/atomic"
)

// logHook receives every operator line after the standard logger wrote it.
var logHook atomic.Pointer[func(string)]

// SetLogHook registers fn to receive a copy of each [excel-bps] operator line,
// so the gateway can keep them in a file of their own. nil removes the hook.
func SetLogHook(fn func(string)) {
	if fn == nil {
		logHook.Store(nil)
		return
	}
	logHook.Store(&fn)
}

// Logf writes one [excel-bps] operator log line to the standard logger and to
// the registered hook. Lines must name only shapes, counts and identifiers:
// never request content, tool arguments or credentials.
func Logf(format string, args ...any) {
	line := fmt.Sprintf("[excel-bps] "+format, args...)
	_ = log.Output(2, line)
	if hook := logHook.Load(); hook != nil {
		(*hook)(line)
	}
}
