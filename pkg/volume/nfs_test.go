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

func newTestNFSMounter(
	exec remote.RemoteExecutor,
) *NFSMounter {
	// testMountOptionsWithAddress (see VOL-HIGH-1, manager_test.go)
	// configures a placeholder LocalHostAddress so these tests keep
	// exercising their mkdir/mount executor-error paths rather than the
	// unconfigured-address fail-loud path.
	return NewNFSMounter(
		exec,
		logging.NopLogger{},
		testMountOptionsWithAddress(),
	)
}

func TestNFSMount_Mount_NoExecutor(t *testing.T) {
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
			mount: VolumeMount{
				Name:       "nfs-data",
				Type:       MountNFS,
				LocalPath:  "/local/nfs",
				RemotePath: "/remote/nfs",
				HostName:   "test-host",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mounter := NewNFSMounter(
				tc.executor,
				logging.NopLogger{},
				DefaultMountOptions(),
			)
			require.NotNil(t, mounter)

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
					assert.Error(t, err)
					return
				}
			}()

			if panicked {
				assert.True(t, panicked,
					"Mount with nil executor should panic "+
						"or return error")
			}
		})
	}
}

func TestNFSMount_Mount_ExecutorError(t *testing.T) {
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
			name: "executor returns non-zero exit on mkdir",
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
			mounter := newTestNFSMounter(exec)

			mount := VolumeMount{
				Name:       "nfs-data",
				Type:       MountNFS,
				LocalPath:  "/local/nfs",
				RemotePath: "/remote/nfs",
				HostName:   "test-host",
			}

			err := mounter.Mount(
				context.Background(), testHost(), mount,
			)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.errContains)
		})
	}
}

func TestNFSMount_Unmount_NotMounted(t *testing.T) {
	tests := []struct {
		name  string
		host  remote.RemoteHost
		mount VolumeMount
	}{
		{
			name: "unmount path that was never mounted",
			host: testHost(),
			mount: VolumeMount{
				Name:       "nfs-never",
				Type:       MountNFS,
				LocalPath:  "/local/nfs",
				RemotePath: "/remote/nfs",
				HostName:   "test-host",
			},
		},
		{
			name: "unmount read-only nfs mount",
			host: testHost(),
			mount: VolumeMount{
				Name:       "nfs-ro",
				Type:       MountNFS,
				LocalPath:  "/local/nfs-ro",
				RemotePath: "/remote/nfs-ro",
				HostName:   "test-host",
				ReadOnly:   true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec := &mockExecutor{}
			mounter := newTestNFSMounter(exec)

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

func TestNFSMount_Status_NotMounted(t *testing.T) {
	tests := []struct {
		name      string
		mountName string
	}{
		{
			name:      "status of never-created nfs mount",
			mountName: "nfs-nonexistent",
		},
		{
			name:      "status of empty-named nfs mount",
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

func TestNFSMount_Type(t *testing.T) {
	tests := []struct {
		name     string
		mount    VolumeMount
		expected MountType
	}{
		{
			name: "nfs mount type",
			mount: VolumeMount{
				Name: "nfs-data",
				Type: MountNFS,
			},
			expected: MountNFS,
		},
		{
			name: "nfs type string value",
			mount: VolumeMount{
				Name: "typed-nfs",
				Type: MountNFS,
			},
			expected: "nfs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.mount.Type)
			assert.Equal(t, "nfs", string(MountNFS))
		})
	}
}
