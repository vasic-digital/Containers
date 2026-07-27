//go:build !integration

package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLXDRuntime_Status_SelectsExactNameNotSubstringMatch proves Status(id)
// returns the container whose name EXACTLY equals id, not the first row of a
// substring match. `lxc list web` matches "web", "web-staging" and "webapp";
// taking containers[0] blindly reported a DIFFERENT container's state
// (§11.4.111 — resolve by stable name).
func TestLXDRuntime_Status_SelectsExactNameNotSubstringMatch(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			// "web-staging" sorts first; the exact target "web" is second.
			return []byte(`[
				{"name":"web-staging","status":"Stopped","status_code":102},
				{"name":"web","status":"Running","status_code":103}
			]`), nil
		},
	}
	l := NewLXDRuntimeWithExecutor(exec)

	st, err := l.Status(context.Background(), "web")
	require.NoError(t, err)
	require.Equal(t, "web", st.Name,
		"Status returned a substring match instead of the exact-named container")
	assert.Equal(t, StateRunning, st.State,
		"Status reported a DIFFERENT container's state (row 0, web-staging)")
}

// TestLXDRuntime_Status_NoExactMatchErrors proves Status errors when no
// container matches the id exactly, even though the substring filter returned
// rows (a partial-name neighbour must not be silently accepted).
func TestLXDRuntime_Status_NoExactMatchErrors(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(`[{"name":"web-staging","status":"Running","status_code":103}]`), nil
		},
	}
	l := NewLXDRuntimeWithExecutor(exec)

	_, err := l.Status(context.Background(), "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no container found")
}

// TestAutoDetect_HonoursConfiguredRuntimePriority proves AutoDetect consults
// RuntimePriority. Two available runtimes; the factory order is [alpha, beta],
// but the configured priority prefers beta — AutoDetect must return beta.
// Pre-fix, AutoDetect ignored RuntimePriority and always used the hardcoded
// factory order, returning alpha; SetRuntimePriority was a documented no-op.
func TestAutoDetect_HonoursConfiguredRuntimePriority(t *testing.T) {
	origFactory := detectFactory
	origPriority := GetRuntimePriority()
	t.Cleanup(func() {
		detectFactory = origFactory
		SetRuntimePriority(origPriority)
	})

	detectFactory = func() []ContainerRuntime {
		return []ContainerRuntime{
			&availableRuntime{name: "alpha", available: true},
			&availableRuntime{name: "beta", available: true},
		}
	}
	SetRuntimePriority([]string{"beta", "alpha"})

	rt, err := AutoDetect(context.Background())
	require.NoError(t, err)
	if rt.Name() != "beta" {
		t.Fatalf("AutoDetect returned %q; it ignored the configured "+
			"RuntimePriority [beta, alpha] and used the hardcoded factory order",
			rt.Name())
	}
}

// TestCRIORuntime_List_HonoursNameFilter proves crio List applies the caller's
// name filter. Pre-fix, parseCrioPods ignored filter.Names/Labels/Status and
// returned EVERY pod, so a caller asking for one service got the whole cluster.
func TestCRIORuntime_List_HonoursNameFilter(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(`[
				{"id":"1","name":"web","state":"SANDBOX_READY","labels":{}},
				{"id":"2","name":"db","state":"SANDBOX_READY","labels":{}}
			]`), nil
		},
	}
	c := NewCRIORuntimeWithExecutor(exec)

	infos, err := c.List(context.Background(), ListFilter{Names: []string{"web"}})
	require.NoError(t, err)
	require.Len(t, infos, 1,
		"List ignored the Names filter and returned every pod")
	assert.Equal(t, "web", infos[0].Name)
}

// TestCRIORuntime_List_HonoursLabelFilter proves crio List applies the label
// filter too (every filter label must be present with an equal value).
func TestCRIORuntime_List_HonoursLabelFilter(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(`[
				{"id":"1","name":"a","state":"SANDBOX_READY","labels":{"tier":"web"}},
				{"id":"2","name":"b","state":"SANDBOX_READY","labels":{"tier":"db"}}
			]`), nil
		},
	}
	c := NewCRIORuntimeWithExecutor(exec)

	infos, err := c.List(context.Background(),
		ListFilter{Labels: map[string]string{"tier": "web"}})
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, "a", infos[0].Name)
}
