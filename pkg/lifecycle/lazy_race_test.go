package lifecycle_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/lifecycle"

	"github.com/stretchr/testify/assert"
)

// TestLazyBooter_GetError_ConcurrentWithEnsureStarted reproduces a data
// race on the LazyBooter.err field. EnsureStarted writes lb.err inside
// once.Do while a concurrent GetError reader reads lb.err with no
// synchronization. Run with -race to expose the unsynchronized access.
//
// This is a genuine product defect: GetError() / EnsureStarted()'s
// fast-path return value can be torn / report a stale-or-garbage error,
// meaning a caller polling GetError() during a slow boot may observe a
// "no error" reading while the boot actually failed (a false-success /
// PASS-bluff for the lifecycle layer), or trip the race detector and
// crash a -race build.
func TestLazyBooter_GetError_ConcurrentWithEnsureStarted(t *testing.T) {
	bootErr := errors.New("boot failed")
	lb := lifecycle.NewLazyBooter(func() error {
		// Hold the write window open so the reader overlaps the
		// lb.err assignment.
		time.Sleep(2 * time.Millisecond)
		return bootErr
	})

	var wg sync.WaitGroup

	// Writer: triggers the slow path that assigns lb.err.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = lb.EnsureStarted()
	}()

	// Readers: hammer GetError concurrently with the err assignment.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = lb.GetError()
			}
		}()
	}

	wg.Wait()

	// After everything settles, the error MUST be observable correctly.
	assert.ErrorIs(t, lb.GetError(), bootErr)
}
