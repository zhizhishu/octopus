// Package safe provides guarded goroutine helpers so a panic in a background
// goroutine never crashes the whole process.
package safe

import (
	"runtime/debug"

	"github.com/bestruirui/octopus/internal/utils/log"
)

// SafeGo runs fn in a new goroutine with a panic guard. gin.CustomRecovery only
// covers the HTTP handler chain; a panic in any other goroutine would kill the
// entire gateway and every in-flight stream with it (Go runtime semantics).
// SafeGo logs the panic with a stack trace and does not re-panic.
//
// Defers inside fn (defer close(ch), defer wg.Done(), ...) run before the
// guard's recover, exactly as with a bare `go`, so consumers blocked on those
// signals are still released when fn panics.
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("panic in goroutine %s: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}
