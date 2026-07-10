//go:build !integration

package runtime

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCRIORuntime_Name(t *testing.T) {
	c := NewCRIORuntimeWithExecutor(&mockExecutor{})
	assert.Equal(t, "cri-o", c.Name())
}

func TestCRIORuntime_NewCRIORuntime(t *testing.T) {
	c := NewCRIORuntime()
	assert.NotNil(t, c)
	assert.Equal(t, "crictl", c.binary)
}

func TestCRIORuntime_Version(t *testing.T) {
	t.Run("json response", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(`{"runtimeName":"cri-o","runtimeVersion":"1.27.0"}`), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		ver, err := c.Version(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "1.27.0", ver)
	})

	t.Run("non-json fallback", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte("1.27.0\n"), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		ver, err := c.Version(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "1.27.0", ver)
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, fmt.Errorf("crictl not found")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		_, err := c.Version(context.Background())
		assert.Error(t, err)
	})
}

func TestCRIORuntime_IsAvailable(t *testing.T) {
	t.Run("available", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(`{}`), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		assert.True(t, c.IsAvailable(context.Background()))
	})

	t.Run("not available", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, fmt.Errorf("not found")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		assert.False(t, c.IsAvailable(context.Background()))
	})
}

func TestCRIORuntime_Start(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(""), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		err := c.Start(context.Background(), "container-id")
		assert.NoError(t, err)
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, fmt.Errorf("start failed")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		err := c.Start(context.Background(), "container-id")
		assert.Error(t, err)
	})
}

func TestCRIORuntime_Stop(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(""), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		err := c.Stop(context.Background(), "container-id")
		assert.NoError(t, err)
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, fmt.Errorf("stop failed")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		err := c.Stop(context.Background(), "container-id")
		assert.Error(t, err)
	})
}

func TestCRIORuntime_Remove(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(""), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		err := c.Remove(context.Background(), "container-id")
		assert.NoError(t, err)
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, fmt.Errorf("remove failed")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		err := c.Remove(context.Background(), "container-id")
		assert.Error(t, err)
	})
}

func TestCRIORuntime_Status(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		output := `{"status":{"state":"running","startedAt":"2024-01-01T00:00:00Z"},"info":{"runtimeSpec":{"process":{"args":["/bin/sh"]}}}}`
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(output), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		status, err := c.Status(context.Background(), "container-id")
		require.NoError(t, err)
		assert.Equal(t, StateRunning, status.State)
	})

	t.Run("stopped", func(t *testing.T) {
		output := `{"status":{"state":"stopped","startedAt":""},"info":{}}`
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(output), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		status, err := c.Status(context.Background(), "container-id")
		require.NoError(t, err)
		assert.Equal(t, StateStopped, status.State)
	})

	t.Run("paused", func(t *testing.T) {
		output := `{"status":{"state":"paused","startedAt":""},"info":{}}`
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(output), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		status, err := c.Status(context.Background(), "container-id")
		require.NoError(t, err)
		assert.Equal(t, StatePaused, status.State)
	})

	t.Run("created", func(t *testing.T) {
		output := `{"status":{"state":"created","startedAt":""},"info":{}}`
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(output), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		status, err := c.Status(context.Background(), "container-id")
		require.NoError(t, err)
		assert.Equal(t, StateCreated, status.State)
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, fmt.Errorf("inspect failed")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		_, err := c.Status(context.Background(), "container-id")
		assert.Error(t, err)
	})
}

func TestCRIORuntime_List(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		output := `[{"id":"pod1","name":"my-pod","state":"SANDBOX_READY","labels":{},"createdAt":1704067200000000000}]`
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(output), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		containers, err := c.List(context.Background(), ListFilter{All: true})
		require.NoError(t, err)
		assert.Len(t, containers, 1)
		assert.Equal(t, "pod1", containers[0].ID)
		assert.Equal(t, "my-pod", containers[0].Name)
	})

	t.Run("empty list", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(`[]`), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		containers, err := c.List(context.Background(), ListFilter{})
		require.NoError(t, err)
		assert.Empty(t, containers)
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, fmt.Errorf("pods failed")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		_, err := c.List(context.Background(), ListFilter{})
		assert.Error(t, err)
	})
}

func TestCRIORuntime_Stats(t *testing.T) {
	t.Run("success with stats", func(t *testing.T) {
		output := `{"stats":[{"cpu":"10%","memory":{"workingSetBytes":"104857600"}}]}`
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(output), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		stats, err := c.Stats(context.Background(), "container-id")
		require.NoError(t, err)
		assert.Equal(t, uint64(104857600), stats.MemoryUsage)
	})

	t.Run("empty stats", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(`{"stats":[]}`), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		stats, err := c.Stats(context.Background(), "container-id")
		require.NoError(t, err)
		assert.NotNil(t, stats)
	})

	t.Run("invalid json falls back to empty", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(`not-json`), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		stats, err := c.Stats(context.Background(), "container-id")
		require.NoError(t, err)
		assert.NotNil(t, stats)
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return nil, fmt.Errorf("stats failed")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		_, err := c.Stats(context.Background(), "container-id")
		assert.Error(t, err)
	})
}

func TestCRIORuntime_Exec(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		exec := &mockExecutor{
			executeWithStderrFunc: func(
				ctx context.Context, name string, args ...string,
			) ([]byte, []byte, int, error) {
				return []byte("output"), []byte(""), 0, nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		result, err := c.Exec(context.Background(), "container-id", []string{"ls", "-la"})
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
		assert.Equal(t, "output", result.Stdout)
	})

	t.Run("non-zero exit", func(t *testing.T) {
		exec := &mockExecutor{
			executeWithStderrFunc: func(
				ctx context.Context, name string, args ...string,
			) ([]byte, []byte, int, error) {
				return []byte(""), []byte("error msg"), 1, nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		result, err := c.Exec(context.Background(), "container-id", []string{"false"})
		require.NoError(t, err)
		assert.Equal(t, 1, result.ExitCode)
		assert.Equal(t, "error msg", result.Stderr)
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeWithStderrFunc: func(
				ctx context.Context, name string, args ...string,
			) ([]byte, []byte, int, error) {
				return nil, nil, -1, fmt.Errorf("exec failed")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		_, err := c.Exec(context.Background(), "container-id", []string{"cmd"})
		assert.Error(t, err)
	})
}

func TestCRIORuntime_Logs(t *testing.T) {
	t.Run("success no options", func(t *testing.T) {
		exec := &mockExecutor{
			executeStreamFunc: func(
				ctx context.Context, name string, args ...string,
			) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("log line")), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		rc, err := c.Logs(context.Background(), "container-id")
		require.NoError(t, err)
		defer rc.Close()
	})

	t.Run("success with follow and tail", func(t *testing.T) {
		exec := &mockExecutor{
			executeStreamFunc: func(
				ctx context.Context, name string, args ...string,
			) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("log line")), nil
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		rc, err := c.Logs(context.Background(), "container-id",
			WithFollow(true), WithTail("100"))
		require.NoError(t, err)
		defer rc.Close()
	})

	t.Run("error", func(t *testing.T) {
		exec := &mockExecutor{
			executeStreamFunc: func(
				ctx context.Context, name string, args ...string,
			) (io.ReadCloser, error) {
				return nil, fmt.Errorf("stream failed")
			},
		}
		c := NewCRIORuntimeWithExecutor(exec)
		_, err := c.Logs(context.Background(), "container-id")
		assert.Error(t, err)
	})
}

func TestMapCrioState(t *testing.T) {
	assert.Equal(t, StateRunning, mapCrioState("running"))
	assert.Equal(t, StateStopped, mapCrioState("stopped"))
	assert.Equal(t, StateStopped, mapCrioState("exited"))
	assert.Equal(t, StatePaused, mapCrioState("paused"))
	assert.Equal(t, StateCreated, mapCrioState("created"))
	assert.Equal(t, ContainerState("unknown"), mapCrioState("unknown"))
}

func TestParseCrioVersion_InvalidJSON(t *testing.T) {
	// When JSON is invalid, falls back to the raw string.
	ver, err := parseCrioVersion([]byte("not-json"))
	require.NoError(t, err)
	assert.Equal(t, "not-json", ver)
}

func TestParseCrioPods_InvalidJSON(t *testing.T) {
	_, err := parseCrioPods([]byte("not-json"), ListFilter{})
	assert.Error(t, err)
}

func TestParseCrioInspect_InvalidJSON(t *testing.T) {
	_, err := parseCrioInspect([]byte("not-json"))
	assert.Error(t, err)
}
