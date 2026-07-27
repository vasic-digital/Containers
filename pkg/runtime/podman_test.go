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

func TestPodmanRuntime_Name(t *testing.T) {
	p := NewPodmanRuntimeWithExecutor(&mockExecutor{})
	assert.Equal(t, "podman", p.Name())
}

func TestPodmanRuntime_Version(t *testing.T) {
	tests := []struct {
		name      string
		callCount int
		outputs   []string
		errors    []error
		want      string
		wantErr   bool
	}{
		{
			name:      "server version success",
			callCount: 0,
			outputs:   []string{"4.8.0\n"},
			errors:    []error{nil},
			want:      "4.8.0",
		},
		{
			name:      "fallback to client version",
			callCount: 0,
			outputs:   []string{"", "4.7.0\n"},
			errors:    []error{fmt.Errorf("no server"), nil},
			want:      "4.7.0",
		},
		{
			name:    "both fail",
			outputs: []string{"", ""},
			errors: []error{
				fmt.Errorf("no server"),
				fmt.Errorf("no client"),
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callIdx := 0
			exec := &mockExecutor{
				executeFunc: func(
					_ context.Context, _ string, _ ...string,
				) ([]byte, error) {
					idx := callIdx
					callIdx++
					if idx < len(tt.outputs) {
						return []byte(tt.outputs[idx]),
							tt.errors[idx]
					}
					return nil, fmt.Errorf("unexpected call")
				},
			}
			p := NewPodmanRuntimeWithExecutor(exec)
			got, err := p.Version(context.Background())
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPodmanRuntime_IsAvailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"available", nil, true},
		{"not available", fmt.Errorf("not found"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &mockExecutor{
				executeFunc: func(
					_ context.Context, _ string, _ ...string,
				) ([]byte, error) {
					return nil, tt.err
				},
			}
			p := NewPodmanRuntimeWithExecutor(exec)
			assert.Equal(t, tt.want, p.IsAvailable(context.Background()))
		})
	}
}

func TestPodmanRuntime_Start(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"success", nil, false},
		{"error", fmt.Errorf("fail"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &mockExecutor{
				executeFunc: func(
					_ context.Context, _ string, _ ...string,
				) ([]byte, error) {
					return nil, tt.err
				},
			}
			p := NewPodmanRuntimeWithExecutor(exec)
			err := p.Start(context.Background(), "test-pod")
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPodmanRuntime_Stop(t *testing.T) {
	var capturedArgs []string
	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, args ...string,
		) ([]byte, error) {
			capturedArgs = args
			return nil, nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	err := p.Stop(context.Background(), "test-pod")
	require.NoError(t, err)
	assert.Contains(t, capturedArgs, "stop")
	assert.Contains(t, capturedArgs, "test-pod")
}

func TestPodmanRuntime_Remove(t *testing.T) {
	tests := []struct {
		name    string
		force   bool
		volumes bool
		wantF   bool
		wantV   bool
	}{
		{"simple", false, false, false, false},
		{"force", true, false, true, false},
		{"force and volumes", true, true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedArgs []string
			exec := &mockExecutor{
				executeFunc: func(
					_ context.Context, _ string, args ...string,
				) ([]byte, error) {
					capturedArgs = args
					return nil, nil
				},
			}
			p := NewPodmanRuntimeWithExecutor(exec)
			var opts []RemoveOption
			if tt.force {
				opts = append(opts, WithForceRemove(true))
			}
			if tt.volumes {
				opts = append(opts, WithRemoveVolumes(true))
			}
			err := p.Remove(
				context.Background(), "test-pod", opts...,
			)
			require.NoError(t, err)
			if tt.wantF {
				assert.Contains(t, capturedArgs, "-f")
			}
			if tt.wantV {
				assert.Contains(t, capturedArgs, "-v")
			}
		})
	}
}

func TestPodmanRuntime_Status(t *testing.T) {
	inspectJSON := `[{
		"Id": "pod123",
		"Name": "/my-pod",
		"State": {
			"Status": "running",
			"Running": true,
			"ExitCode": 0,
			"StartedAt": "2024-01-15T10:30:00Z",
			"FinishedAt": "0001-01-01T00:00:00Z"
		},
		"Config": {"Labels": {}, "Image": "alpine"},
		"Image": "sha256:def",
		"Created": "2024-01-15T10:00:00Z",
		"NetworkSettings": {"Networks": {}, "Ports": {}}
	}]`

	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return []byte(inspectJSON), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	status, err := p.Status(context.Background(), "my-pod")
	require.NoError(t, err)
	assert.Equal(t, "pod123", status.ID)
	assert.Equal(t, StateRunning, status.State)
}

func TestPodmanRuntime_List(t *testing.T) {
	podmanPS := `[
		{
			"Id": "abc123",
			"Names": ["web"],
			"Image": "nginx",
			"ImageID": "sha256:abc",
			"State": "running",
			"Status": "Up 2 hours",
			"Labels": {"app": "web"}
		},
		{
			"Id": "def456",
			"Names": ["db"],
			"Image": "postgres",
			"ImageID": "sha256:def",
			"State": "exited",
			"Status": "Exited (0) 1 hour ago",
			"Labels": {"app": "db"}
		}
	]`

	tests := []struct {
		name    string
		output  string
		err     error
		wantErr bool
		wantLen int
	}{
		{
			name:    "two containers",
			output:  podmanPS,
			wantLen: 2,
		},
		{
			name:    "empty array",
			output:  "[]",
			wantLen: 0,
		},
		{
			name:    "empty output",
			output:  "",
			wantLen: 0,
		},
		{
			name:    "command error",
			err:     fmt.Errorf("fail"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &mockExecutor{
				executeFunc: func(
					_ context.Context, _ string, _ ...string,
				) ([]byte, error) {
					return []byte(tt.output), tt.err
				},
			}
			p := NewPodmanRuntimeWithExecutor(exec)
			containers, err := p.List(
				context.Background(), ListFilter{All: true},
			)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, containers, tt.wantLen)
		})
	}
}

func TestPodmanRuntime_Stats(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		err     error
		wantErr bool
		wantCPU float64
	}{
		{
			// §11.4.107(10): real `podman stats --format json` — a JSON array
			// of objects whose fields are STRINGS, with combined net_io/block_io
			// and a combined mem_usage "used / limit". The prior fixture fed a
			// fabricated numeric shape no runtime emits (the fixture-bluff).
			name: "podman native format (real string-array shape)",
			output: `[{
				"id": "abc123",
				"name": "test-pod",
				"cpu_percent": "2.50%",
				"avg_cpu": "2.00%",
				"mem_usage": "100MB / 1GB",
				"mem_percent": "10.00%",
				"net_io": "1kB / 2kB",
				"block_io": "512B / 1kB",
				"pids": "5"
			}]`,
			wantCPU: 2.5,
		},
		{
			name:    "empty output",
			output:  "",
			wantErr: true,
		},
		{
			name:    "command error",
			err:     fmt.Errorf("fail"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &mockExecutor{
				executeFunc: func(
					_ context.Context, _ string, _ ...string,
				) ([]byte, error) {
					return []byte(tt.output), tt.err
				},
			}
			p := NewPodmanRuntimeWithExecutor(exec)
			stats, err := p.Stats(
				context.Background(), "test-pod",
			)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, tt.wantCPU, stats.CPUPercent, 0.01)
		})
	}
}

func TestPodmanRuntime_Exec(t *testing.T) {
	exec := &mockExecutor{
		executeWithStderrFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, []byte, int, error) {
			return []byte("output"), []byte(""), 0, nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	result, err := p.Exec(
		context.Background(), "test-pod", []string{"ls", "-la"},
	)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "output", result.Stdout)
}

func TestPodmanRuntime_Logs(t *testing.T) {
	exec := &mockExecutor{
		executeStreamFunc: func(
			_ context.Context, _ string, _ ...string,
		) (io.ReadCloser, error) {
			return io.NopCloser(
				strings.NewReader("log output"),
			), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	rc, err := p.Logs(
		context.Background(), "test-pod",
		WithFollow(true), WithTail("100"),
	)
	require.NoError(t, err)
	defer rc.Close()
	data, readErr := io.ReadAll(rc)
	require.NoError(t, readErr)
	assert.Equal(t, "log output", string(data))
}

func TestPodmanRuntime_Stop_Error(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return nil, fmt.Errorf("stop failed")
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	err := p.Stop(context.Background(), "test-pod")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "podman stop")
}

func TestPodmanRuntime_Remove_Error(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return nil, fmt.Errorf("remove failed")
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	err := p.Remove(context.Background(), "test-pod")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "podman rm")
}

func TestPodmanRuntime_Status_Error(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return nil, fmt.Errorf("inspect failed")
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	_, err := p.Status(context.Background(), "test-pod")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "podman inspect")
}

func TestPodmanRuntime_List_WithFilters(t *testing.T) {
	podmanPS := `[{"Id": "abc", "Names": ["web"], "Image": "nginx", "ImageID": "sha256:abc", "State": "running", "Status": "Up", "Labels": {}}]`

	var capturedArgs []string
	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, args ...string,
		) ([]byte, error) {
			capturedArgs = args
			return []byte(podmanPS), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)

	filter := ListFilter{
		All:    true,
		Labels: map[string]string{"app": "web"},
		Names:  []string{"web"},
		Status: []ContainerState{StateRunning},
	}

	_, err := p.List(context.Background(), filter)
	require.NoError(t, err)

	assert.Contains(t, capturedArgs, "-a")
	assert.Contains(t, capturedArgs, "--filter")
}

func TestPodmanRuntime_List_InvalidJSON(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return []byte("not-json"), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	_, err := p.List(context.Background(), ListFilter{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parsing podman ps output")
}

func TestPodmanRuntime_List_NilLabels(t *testing.T) {
	// Container with null labels should get empty map.
	podmanPS := `[{"Id": "abc", "Names": ["web"], "Image": "nginx", "ImageID": "sha256:abc", "State": "running", "Status": "Up", "Labels": null}]`

	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return []byte(podmanPS), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	containers, err := p.List(context.Background(), ListFilter{})
	require.NoError(t, err)
	require.Len(t, containers, 1)
	assert.NotNil(t, containers[0].Labels)
}

func TestPodmanRuntime_List_NoNames(t *testing.T) {
	// Container with empty names array.
	podmanPS := `[{"Id": "abc", "Names": [], "Image": "nginx", "ImageID": "sha256:abc", "State": "running", "Status": "Up", "Labels": {}}]`

	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return []byte(podmanPS), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	containers, err := p.List(context.Background(), ListFilter{})
	require.NoError(t, err)
	require.Len(t, containers, 1)
	assert.Empty(t, containers[0].Name)
}

func TestPodmanRuntime_Stats_EmptyArray(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return []byte("[]"), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	_, err := p.Stats(context.Background(), "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no stats data")
}

func TestPodmanRuntime_Stats_FallbackToDockerFormat(t *testing.T) {
	// When podman returns docker-style string format, it should fall back.
	dockerStats := `{"CPUPerc":"1.0%","MemPerc":"5.0%","MemUsage":"50MiB / 500MiB","NetIO":"1kB / 1kB","BlockIO":"0B / 0B","PIDs":"2"}`

	exec := &mockExecutor{
		executeFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, error) {
			return []byte(dockerStats), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	stats, err := p.Stats(context.Background(), "test")
	require.NoError(t, err)
	assert.InDelta(t, 1.0, stats.CPUPercent, 0.01)
}

func TestPodmanRuntime_Exec_Error(t *testing.T) {
	exec := &mockExecutor{
		executeWithStderrFunc: func(
			_ context.Context, _ string, _ ...string,
		) ([]byte, []byte, int, error) {
			return nil, nil, 0, fmt.Errorf("exec failed")
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	_, err := p.Exec(context.Background(), "test", []string{"ls"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "podman exec")
}

func TestPodmanRuntime_Logs_Error(t *testing.T) {
	exec := &mockExecutor{
		executeStreamFunc: func(
			_ context.Context, _ string, _ ...string,
		) (io.ReadCloser, error) {
			return nil, fmt.Errorf("logs failed")
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	_, err := p.Logs(context.Background(), "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "podman logs")
}

func TestPodmanRuntime_Logs_AllOptions(t *testing.T) {
	var capturedArgs []string
	exec := &mockExecutor{
		executeStreamFunc: func(
			_ context.Context, _ string, args ...string,
		) (io.ReadCloser, error) {
			capturedArgs = args
			return io.NopCloser(strings.NewReader("")), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)
	rc, err := p.Logs(
		context.Background(), "test",
		WithFollow(true),
		WithSince("1h"),
		WithUntil("30m"),
		WithTail("50"),
	)
	require.NoError(t, err)
	defer rc.Close()

	assert.Contains(t, capturedArgs, "-f")
	assert.Contains(t, capturedArgs, "--since")
	assert.Contains(t, capturedArgs, "--until")
	assert.Contains(t, capturedArgs, "--tail")
}
