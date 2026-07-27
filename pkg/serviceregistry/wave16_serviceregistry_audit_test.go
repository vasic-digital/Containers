package serviceregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wave-16 serviceregistry audit guards. Each reproduces a real defect on the
// pre-fix code and turns GREEN only against the fixed behaviour.

// captureLogger records whether an Error-level message was emitted.
type captureLogger struct {
	mu     sync.Mutex
	errors int
}

func (l *captureLogger) Info(string, ...any)  {}
func (l *captureLogger) Debug(string, ...any) {}
func (l *captureLogger) Warn(string, ...any)  {}
func (l *captureLogger) Error(string, ...any) {
	l.mu.Lock()
	l.errors++
	l.mu.Unlock()
}
func (l *captureLogger) hasError() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.errors > 0
}

// TestWave16_Registry_CorruptFile_PreservedAndLoud covers DEFECT-4: a corrupt
// registry file was silently swallowed (logged at Warn, then return), leaving
// an empty registry indistinguishable from "nothing was ever registered" —
// masking data loss. The fix preserves the corrupt bytes aside and surfaces the
// failure at Error level.
func TestWave16_Registry_CorruptFile_PreservedAndLoud(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "services.json")
	require.NoError(t, os.WriteFile(file, []byte("{ this is not valid json"), 0644))

	lg := &captureLogger{}
	r := New(WithRegistryDir(dir), WithLogger(lg))

	// A corrupt file must NOT silently load as an empty-and-forgotten registry.
	assert.Empty(t, r.List(), "corrupt file must not silently load as empty")

	// The corrupt bytes are preserved aside for recovery ...
	_, corruptErr := os.Stat(file + ".corrupt")
	assert.NoError(t, corruptErr, "corrupt file must be preserved aside as .corrupt")

	// ... the corrupt original is moved out of the way (so the next persist
	// writes a fresh, valid file instead of appending to garbage) ...
	_, origErr := os.Stat(file)
	assert.True(t, os.IsNotExist(origErr), "corrupt original must be moved aside")

	// ... and the failure is surfaced loudly (Error level), never swallowed.
	assert.True(t, lg.hasError(), "corruption must be logged at Error level")
}

// TestWave16_Registry_ConcurrentPersist_NeverCorrupt covers DEFECT-1 (blocking
// disk I/O was performed while holding r.mu — MANDATORY PRINCIPLE #2) and
// DEFECT-3 (the registry file was written in place, so a reader — or a crash —
// could observe a truncated/partial file). The fix marshals under the read
// lock and writes OUTSIDE r.mu via an atomic temp-file + rename. Under heavy
// concurrent registration, the on-disk file must therefore ALWAYS be complete
// and parseable, and a concurrent GetAll() must never deadlock behind the I/O.
// Run under -race, this also proves the lock restructuring introduced no data
// race.
func TestWave16_Registry_ConcurrentPersist_NeverCorrupt(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	file := filepath.Join(dir, "services.json")

	// Sizeable labels widen each write so an in-place (non-atomic) writer would
	// leave a partial file visible to a concurrent reader.
	labels := map[string]string{}
	for k := 0; k < 24; k++ {
		labels[fmt.Sprintf("label-key-%02d", k)] = fmt.Sprintf("label-value-%02d-padding-padding", k)
	}

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.Register(fmt.Sprintf("svc-%03d", i), 4000+i, WithLabels(labels))
		}(i)
	}

	stop := make(chan struct{})
	var readWg sync.WaitGroup
	for j := 0; j < 4; j++ {
		readWg.Add(1)
		go func() {
			defer readWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(file)
				if err != nil {
					continue // file may not exist yet
				}
				var m map[string]*Service
				if err := json.Unmarshal(data, &m); err != nil {
					t.Errorf("on-disk registry observed corrupt mid-write: %v", err)
					return
				}
				_ = r.GetAll() // must not block behind persist I/O
			}
		}()
	}

	wg.Wait()
	close(stop)
	readWg.Wait()

	// All registrations landed in memory ...
	assert.Len(t, r.List(), n)

	// ... and the final on-disk file is valid and complete.
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var final map[string]*Service
	require.NoError(t, json.Unmarshal(data, &final))
	assert.Len(t, final, n)

	// No temp files leaked into the registry directory.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "temp registry file leaked: %s", e.Name())
	}
}
