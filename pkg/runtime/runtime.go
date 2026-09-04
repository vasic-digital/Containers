package runtime

import (
	"context"
	"io"
)

// ContainerRuntime defines the interface for interacting with a
// container runtime such as Docker, Podman, or Kubernetes.
type ContainerRuntime interface {
	// Name returns the runtime name (e.g., "docker", "podman", "kubernetes").
	Name() string

	// Version returns the runtime server version string.
	Version(ctx context.Context) (string, error)

	// IsAvailable checks whether the runtime is reachable and operational.
	IsAvailable(ctx context.Context) bool

	// Start starts a container identified by its ID or name.
	Start(ctx context.Context, id string, opts ...StartOption) error

	// Stop stops a running container.
	Stop(ctx context.Context, id string, opts ...StopOption) error

	// Remove removes a container.
	Remove(ctx context.Context, id string, opts ...RemoveOption) error

	// Status returns the current status of a container.
	Status(ctx context.Context, id string) (*ContainerStatus, error)

	// List returns containers matching the given filter criteria.
	List(ctx context.Context, filter ListFilter) ([]ContainerInfo, error)

	// Stats returns resource usage statistics for a container.
	Stats(ctx context.Context, id string) (*ContainerStats, error)

	// Exec runs a command inside a running container and returns the result.
	Exec(ctx context.Context, id string, cmd []string) (*ExecResult, error)

	// Run creates a container from image, runs cmd in it, waits for it to
	// exit and (unless WithRunRemove(false)) removes it — the CLI's
	// `run --rm` in one call. Pass WithRunStdin to stream a document into the
	// container's standard input, which is the shape a one-shot filter needs.
	//
	// A non-zero container exit is reported in ExecResult.ExitCode with a nil
	// error, matching Exec. A non-nil error means the run could not be
	// performed at all; runtimes with no equivalent one-shot form return an
	// error wrapping ErrRunUnsupported rather than pretending to succeed.
	Run(
		ctx context.Context, image string, cmd []string, opts ...RunOption,
	) (*ExecResult, error)

	// Logs returns a reader for the container's log output.
	Logs(ctx context.Context, id string, opts ...LogOption) (io.ReadCloser, error)
}
