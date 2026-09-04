//go:build !integration

package runtime

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// availableRuntime is a mock runtime for testing detection logic.
type availableRuntime struct {
	name      string
	available bool
}

func (a *availableRuntime) Name() string { return a.name }
func (a *availableRuntime) Version(
	_ context.Context,
) (string, error) {
	return "1.0.0", nil
}
func (a *availableRuntime) IsAvailable(_ context.Context) bool {
	return a.available
}
func (a *availableRuntime) Start(
	_ context.Context, _ string, _ ...StartOption,
) error {
	return nil
}
func (a *availableRuntime) Stop(
	_ context.Context, _ string, _ ...StopOption,
) error {
	return nil
}
func (a *availableRuntime) Remove(
	_ context.Context, _ string, _ ...RemoveOption,
) error {
	return nil
}
func (a *availableRuntime) Status(
	_ context.Context, _ string,
) (*ContainerStatus, error) {
	return nil, nil
}
func (a *availableRuntime) List(
	_ context.Context, _ ListFilter,
) ([]ContainerInfo, error) {
	return nil, nil
}
func (a *availableRuntime) Stats(
	_ context.Context, _ string,
) (*ContainerStats, error) {
	return nil, nil
}
func (a *availableRuntime) Exec(
	_ context.Context, _ string, _ []string,
) (*ExecResult, error) {
	return nil, nil
}
func (a *availableRuntime) Logs(
	_ context.Context, _ string, _ ...LogOption,
) (io.ReadCloser, error) {
	return nil, nil
}

func TestAutoDetectWith_DockerFirst(t *testing.T) {
	runtimes := []ContainerRuntime{
		&availableRuntime{name: "docker", available: true},
		&availableRuntime{name: "podman", available: true},
		&availableRuntime{name: "kubernetes", available: true},
	}
	rt, err := autoDetectWith(context.Background(), runtimes)
	require.NoError(t, err)
	assert.Equal(t, "docker", rt.Name())
}

func TestAutoDetectWith_FallbackToPodman(t *testing.T) {
	runtimes := []ContainerRuntime{
		&availableRuntime{name: "docker", available: false},
		&availableRuntime{name: "podman", available: true},
		&availableRuntime{name: "kubernetes", available: false},
	}
	rt, err := autoDetectWith(context.Background(), runtimes)
	require.NoError(t, err)
	assert.Equal(t, "podman", rt.Name())
}

func TestAutoDetectWith_FallbackToKubernetes(t *testing.T) {
	runtimes := []ContainerRuntime{
		&availableRuntime{name: "docker", available: false},
		&availableRuntime{name: "podman", available: false},
		&availableRuntime{name: "kubernetes", available: true},
	}
	rt, err := autoDetectWith(context.Background(), runtimes)
	require.NoError(t, err)
	assert.Equal(t, "kubernetes", rt.Name())
}

func TestAutoDetectWith_NoneAvailable(t *testing.T) {
	runtimes := []ContainerRuntime{
		&availableRuntime{name: "docker", available: false},
		&availableRuntime{name: "podman", available: false},
		&availableRuntime{name: "kubernetes", available: false},
	}
	rt, err := autoDetectWith(context.Background(), runtimes)
	assert.Error(t, err)
	assert.Nil(t, rt)
	assert.Contains(t, err.Error(), "no container runtime detected")
}

func TestAutoDetectWith_EmptyList(t *testing.T) {
	rt, err := autoDetectWith(context.Background(), nil)
	assert.Error(t, err)
	assert.Nil(t, rt)
}

func TestDetectAllWith_AllAvailable(t *testing.T) {
	runtimes := []ContainerRuntime{
		&availableRuntime{name: "docker", available: true},
		&availableRuntime{name: "podman", available: true},
		&availableRuntime{name: "kubernetes", available: true},
	}
	available := detectAllWith(context.Background(), runtimes)
	assert.Len(t, available, 3)
}

func TestDetectAllWith_SomeAvailable(t *testing.T) {
	runtimes := []ContainerRuntime{
		&availableRuntime{name: "docker", available: true},
		&availableRuntime{name: "podman", available: false},
		&availableRuntime{name: "kubernetes", available: true},
	}
	available := detectAllWith(context.Background(), runtimes)
	assert.Len(t, available, 2)
	assert.Equal(t, "docker", available[0].Name())
	assert.Equal(t, "kubernetes", available[1].Name())
}

func TestDetectAllWith_NoneAvailable(t *testing.T) {
	runtimes := []ContainerRuntime{
		&availableRuntime{name: "docker", available: false},
		&availableRuntime{name: "podman", available: false},
		&availableRuntime{name: "kubernetes", available: false},
	}
	available := detectAllWith(context.Background(), runtimes)
	assert.Empty(t, available)
}

func TestDetectAllWith_EmptyInput(t *testing.T) {
	available := detectAllWith(context.Background(), nil)
	assert.Empty(t, available)
}

func TestDefaultRuntimeFactory(t *testing.T) {
	runtimes := defaultRuntimeFactory()
	require.Len(t, runtimes, 6)

	names := make([]string, 0, 6)
	for _, rt := range runtimes {
		names = append(names, rt.Name())
	}

	// Verify priority order: Podman → Docker → nerdctl → CRI-O → LXD → Kubernetes
	assert.Equal(t, []string{"podman", "docker", "nerdctl", "cri-o", "lxd", "kubernetes"}, names)
}

// Tests for the public API using the internal functions.
// These tests verify the integration between public functions and internal logic.

func TestAutoDetect_Integration(t *testing.T) {
	if os.Getenv("CONTAINERS_INTEGRATION_TEST") != "1" {
		t.Skip("Set CONTAINERS_INTEGRATION_TEST=1 to run (execs real runtimes)") // SKIP-OK: #env-integration-only
	}
	ctx := context.Background()

	rt, err := AutoDetect(ctx)
	if err != nil {
		assert.Contains(t, err.Error(), "no container runtime detected")
		assert.Nil(t, rt)
	} else {
		assert.NotNil(t, rt)
		assert.NotEmpty(t, rt.Name())
	}
}

func TestDetectAll_Integration(t *testing.T) {
	if os.Getenv("CONTAINERS_INTEGRATION_TEST") != "1" {
		t.Skip("Set CONTAINERS_INTEGRATION_TEST=1 to run (execs real runtimes)") // SKIP-OK: #env-integration-only
	}
	ctx := context.Background()

	runtimes := DetectAll(ctx)
	for _, rt := range runtimes {
		assert.NotEmpty(t, rt.Name())
	}
}

// Run satisfies the ContainerRuntime interface's ephemeral-run primitive. This
// fake does not exercise it; it returns an empty result rather than a nil one
// so a caller can never dereference nil on a nil error.
func (a *availableRuntime) Run(
	_ context.Context, _ string, _ []string, _ ...RunOption,
) (*ExecResult, error) {
	return &ExecResult{}, nil
}
