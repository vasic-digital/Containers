//go:build !integration

package runtime

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

// alwaysAvailableRuntime is a mock runtime that is always available.
type alwaysAvailableRuntime struct {
	name string
}

func (r *alwaysAvailableRuntime) Name() string { return r.name }
func (r *alwaysAvailableRuntime) Version(ctx context.Context) (string, error) {
	return "1.0", nil
}
func (r *alwaysAvailableRuntime) IsAvailable(ctx context.Context) bool { return true }
func (r *alwaysAvailableRuntime) Start(ctx context.Context, id string, opts ...StartOption) error {
	return nil
}
func (r *alwaysAvailableRuntime) Stop(ctx context.Context, id string, opts ...StopOption) error {
	return nil
}
func (r *alwaysAvailableRuntime) Remove(
	ctx context.Context, id string, opts ...RemoveOption,
) error {
	return nil
}
func (r *alwaysAvailableRuntime) Status(
	ctx context.Context, id string,
) (*ContainerStatus, error) {
	return &ContainerStatus{}, nil
}
func (r *alwaysAvailableRuntime) List(
	ctx context.Context, filter ListFilter,
) ([]ContainerInfo, error) {
	return nil, nil
}
func (r *alwaysAvailableRuntime) Stats(
	ctx context.Context, id string,
) (*ContainerStats, error) {
	return &ContainerStats{}, nil
}
func (r *alwaysAvailableRuntime) Exec(
	ctx context.Context, id string, cmd []string,
) (*ExecResult, error) {
	return &ExecResult{}, nil
}
func (r *alwaysAvailableRuntime) Logs(
	ctx context.Context, id string, opts ...LogOption,
) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}

// neverAvailableRuntime is a mock runtime that is never available.
type neverAvailableRuntime struct {
	name string
}

func (r *neverAvailableRuntime) Name() string { return r.name }
func (r *neverAvailableRuntime) Version(ctx context.Context) (string, error) {
	return "", fmt.Errorf("not available")
}
func (r *neverAvailableRuntime) IsAvailable(ctx context.Context) bool { return false }
func (r *neverAvailableRuntime) Start(
	ctx context.Context, id string, opts ...StartOption,
) error {
	return nil
}
func (r *neverAvailableRuntime) Stop(
	ctx context.Context, id string, opts ...StopOption,
) error {
	return nil
}
func (r *neverAvailableRuntime) Remove(
	ctx context.Context, id string, opts ...RemoveOption,
) error {
	return nil
}
func (r *neverAvailableRuntime) Status(
	ctx context.Context, id string,
) (*ContainerStatus, error) {
	return nil, fmt.Errorf("not available")
}
func (r *neverAvailableRuntime) List(
	ctx context.Context, filter ListFilter,
) ([]ContainerInfo, error) {
	return nil, fmt.Errorf("not available")
}
func (r *neverAvailableRuntime) Stats(
	ctx context.Context, id string,
) (*ContainerStats, error) {
	return nil, fmt.Errorf("not available")
}
func (r *neverAvailableRuntime) Exec(
	ctx context.Context, id string, cmd []string,
) (*ExecResult, error) {
	return nil, fmt.Errorf("not available")
}
func (r *neverAvailableRuntime) Logs(
	ctx context.Context, id string, opts ...LogOption,
) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not available")
}

func TestAutoDetectWith_Found(t *testing.T) {
	rts := []ContainerRuntime{
		&neverAvailableRuntime{name: "podman"},
		&alwaysAvailableRuntime{name: "docker"},
	}
	rt, err := autoDetectWith(context.Background(), rts)
	assert.NoError(t, err)
	assert.Equal(t, "docker", rt.Name())
}

func TestAutoDetectWith_NotFound(t *testing.T) {
	rts := []ContainerRuntime{
		&neverAvailableRuntime{name: "podman"},
		&neverAvailableRuntime{name: "docker"},
	}
	_, err := autoDetectWith(context.Background(), rts)
	assert.Error(t, err)
}

func TestDetectAllWith(t *testing.T) {
	rts := []ContainerRuntime{
		&alwaysAvailableRuntime{name: "podman"},
		&neverAvailableRuntime{name: "docker"},
		&alwaysAvailableRuntime{name: "nerdctl"},
	}
	available := detectAllWith(context.Background(), rts)
	assert.Len(t, available, 2)
}

func TestGetRuntimePriority(t *testing.T) {
	priority := GetRuntimePriority()
	assert.NotEmpty(t, priority)
	assert.Contains(t, priority, "podman")
	assert.Contains(t, priority, "docker")
}

func TestSetRuntimePriority(t *testing.T) {
	original := GetRuntimePriority()
	defer SetRuntimePriority(original)

	SetRuntimePriority([]string{"docker", "podman"})
	priority := GetRuntimePriority()
	assert.Equal(t, []string{"docker", "podman"}, priority)
}

// Run satisfies the ContainerRuntime interface's ephemeral-run primitive. This
// fake does not exercise it; it returns an empty result rather than a nil one
// so a caller can never dereference nil on a nil error.
func (r *alwaysAvailableRuntime) Run(
	_ context.Context, _ string, _ []string, _ ...RunOption,
) (*ExecResult, error) {
	return &ExecResult{}, nil
}

// Run satisfies the ContainerRuntime interface's ephemeral-run primitive. This
// fake does not exercise it; it returns an empty result rather than a nil one
// so a caller can never dereference nil on a nil error.
func (r *neverAvailableRuntime) Run(
	_ context.Context, _ string, _ []string, _ ...RunOption,
) (*ExecResult, error) {
	return &ExecResult{}, nil
}
