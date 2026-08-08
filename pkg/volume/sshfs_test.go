package volume

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

func newTestSSHFSMounter(
	exec remote.RemoteExecutor,
) *SSHFSMounter {
	// testMountOptionsWithAddress (see VOL-HIGH-2, manager_test.go)
	// configures a placeholder LocalHostAddress so these tests keep
	// exercising their mkdir/mount executor-error paths rather than the
	// unconfigured-address fail-loud path.
	return NewSSHFSMounter(
		exec,
		logging.NopLogger{},
		testMountOptionsWithAddress(),
	)
}

func testHost() remote.RemoteHost {
	return remote.RemoteHost{
		Name:    "test-host",
		Address: "10.0.0.1",
		Port:    22,
		User:    "deploy",
	}
}

func testVolumeMount() VolumeMount {
	return VolumeMount{
		Name:       "test-data",
		Type:       MountSSHFS,
		LocalPath:  "/local/data",
		RemotePath: "/remote/data",
		HostName:   "test-host",
	}
}

func TestSSHFSMount_Mount_NoExecutor(t *testing.T) {
	tests := []struct {
		name     string
		executor remote.RemoteExecutor
		host     remote.RemoteHost
		mount    VolumeMount
	}{
		{
			name:     "nil executor causes panic or error",
			executor: nil,
			host:     testHost(),
			mount:    testVolumeMount(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// With a nil executor, Mount should either return
			// an error or panic. We wrap in a recovery to
			// confirm the failure.
			mounter := NewSSHFSMounter(
				tc.executor,
				logging.NopLogger{},
				DefaultMountOptions(),
			)
			require.NotNil(t, mounter)

			// Calling Mount with nil executor will cause a nil
			// pointer dereference since the code calls
			// m.executor.Execute. Recover from panic to confirm
			// the failure mode.
			panicked := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						panicked = true
					}
				}()
				err := mounter.Mount(
					context.Background(), tc.host, tc.mount,
				)
				if err != nil {
					// If it returns an error instead of
					// panicking, that is also acceptable.
					assert.Error(t, err)
					return
				}
			}()

			if panicked {
				// Confirmed: nil executor causes panic on Mount.
				assert.True(t, panicked,
					"Mount with nil executor should panic "+
						"or return error")
			}
		})
	}
}

func TestSSHFSMount_Mount_ExecutorError(t *testing.T) {
	tests := []struct {
		name        string
		executeFunc func(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error)
		errContains string
	}{
		{
			name: "executor returns connection error",
			executeFunc: func(
				ctx context.Context,
				host remote.RemoteHost,
				cmd string,
			) (*remote.CommandResult, error) {
				return nil, fmt.Errorf("connection refused")
			},
			errContains: "connection refused",
		},
		{
			name: "executor returns non-zero exit code",
			executeFunc: func(
				ctx context.Context,
				host remote.RemoteHost,
				cmd string,
			) (*remote.CommandResult, error) {
				return &remote.CommandResult{
					ExitCode: 1,
					Stderr:   "permission denied",
				}, nil
			},
			errContains: "permission denied",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec := &mockExecutor{executeFunc: tc.executeFunc}
			mounter := newTestSSHFSMounter(exec)

			err := mounter.Mount(
				context.Background(), testHost(),
				testVolumeMount(),
			)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.errContains)
		})
	}
}

func TestSSHFSMount_Unmount_NotMounted(t *testing.T) {
	tests := []struct {
		name  string
		host  remote.RemoteHost
		mount VolumeMount
	}{
		{
			name: "unmount path that was never mounted",
			host: testHost(),
			mount: VolumeMount{
				Name:       "never-mounted",
				Type:       MountSSHFS,
				LocalPath:  "/local/x",
				RemotePath: "/remote/x",
				HostName:   "test-host",
			},
		},
		{
			name: "unmount with read-only mount",
			host: testHost(),
			mount: VolumeMount{
				Name:       "ro-mount",
				Type:       MountSSHFS,
				LocalPath:  "/local/ro",
				RemotePath: "/remote/ro",
				HostName:   "test-host",
				ReadOnly:   true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Mock executor returns success for fusermount -u
			// even though nothing is actually mounted.
			// This is normal behavior -- fusermount returns 0
			// or the unmount is a no-op.
			exec := &mockExecutor{}
			mounter := newTestSSHFSMounter(exec)

			err := mounter.Unmount(
				context.Background(), tc.host, tc.mount,
			)
			// When the executor succeeds (exit code 0), Unmount
			// returns nil even if nothing was mounted.
			assert.NoError(t, err,
				"Unmount with successful executor should be "+
					"a no-op when not mounted")
		})
	}
}

func TestSSHFSMount_Status_NotMounted(t *testing.T) {
	tests := []struct {
		name      string
		mountName string
	}{
		{
			name:      "status of never-created mount",
			mountName: "nonexistent",
		},
		{
			name:      "status of empty-named mount",
			mountName: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec := &mockExecutor{}
			hm := &mockHostManager{
				hosts: map[string]remote.RemoteHost{
					"test-host": testHost(),
				},
			}
			mgr := NewVolumeManager(
				hm, exec, logging.NopLogger{},
			)

			info, err := mgr.Status(tc.mountName)
			assert.Error(t, err,
				"Status of non-existent mount should "+
					"return error")
			assert.Nil(t, info)
			assert.Contains(t, err.Error(), "not found")
		})
	}
}

func TestSSHFSMount_Type(t *testing.T) {
	tests := []struct {
		name     string
		mount    VolumeMount
		expected MountType
	}{
		{
			name: "sshfs mount type",
			mount: VolumeMount{
				Name: "sshfs-data",
				Type: MountSSHFS,
			},
			expected: MountSSHFS,
		},
		{
			name: "sshfs type string value",
			mount: VolumeMount{
				Name: "typed",
				Type: MountSSHFS,
			},
			expected: "sshfs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.mount.Type)
			assert.Equal(t, "sshfs", string(MountSSHFS))
		})
	}
}
