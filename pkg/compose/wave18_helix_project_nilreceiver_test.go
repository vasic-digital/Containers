package compose

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHelixComposeProject_NilReceiver_NoPanic is the §11.4.115 guard for
// CT-HARDEN-COMPOSE-1: a nil *HelixComposeProject (a legitimate Go zero value
// reachable via a map-lookup miss, an ignored-error constructor return, or an
// uninitialized struct field) must not crash the caller when GetService/
// ServiceNames/HasService are invoked on it. Pre-fix each dereferenced
// p.Services unconditionally -> SIGSEGV. Fixed: each nil-guards and returns the
// SAME answer an initialized-but-empty project gives.
//
// The oracle asserts BOTH (a) no panic AND (b) the nil-receiver result is
// indistinguishable from a real empty project built via the constructor — the
// equivalence CT-HARDEN-COMPOSE-1 guarantees. The equivalence half is what
// catches a nil-vs-non-nil-empty-slice divergence (e.g. ServiceNames returning
// bare nil while the empty project returns make([]string, 0)).
func TestHelixComposeProject_NilReceiver_NoPanic(t *testing.T) {
	var p *HelixComposeProject                    // legitimate zero value
	empty := NewHelixComposeProject("empty", nil) // real empty-project baseline

	assert.NotPanics(t, func() {
		svc, err := p.GetService("web")
		assert.Nil(t, svc)
		assert.Error(t, err)
		// Indistinguishable from the empty-project case.
		emptySvc, emptyErr := empty.GetService("web")
		assert.Nil(t, emptySvc)
		assert.Error(t, emptyErr)
		assert.Equal(t, emptyErr.Error(), err.Error(),
			"nil-receiver GetService error must match the empty-project error")
	}, "GetService on a nil *HelixComposeProject must not panic")

	assert.NotPanics(t, func() {
		names := p.ServiceNames()
		assert.Empty(t, names)
		emptyNames := empty.ServiceNames()
		// True equivalence: both non-nil empty slices, DeepEqual, same
		// JSON shape ([] not null). A bare-nil return would fail this.
		assert.True(t, reflect.DeepEqual(names, emptyNames),
			"nil-receiver ServiceNames must equal the empty-project result "+
				"(got %#v, empty-project %#v)", names, emptyNames)
		assert.NotNil(t, names,
			"nil-receiver ServiceNames must be a non-nil empty slice, like an empty project")
	}, "ServiceNames on a nil *HelixComposeProject must not panic")

	assert.NotPanics(t, func() {
		got := p.HasService("web")
		assert.False(t, got)
		assert.Equal(t, empty.HasService("web"), got,
			"nil-receiver HasService must match the empty-project result")
	}, "HasService on a nil *HelixComposeProject must not panic")
}
