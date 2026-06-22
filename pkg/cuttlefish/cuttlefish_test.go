package cuttlefish

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedCall is one invocation captured by fakeExecutor.
type recordedCall struct {
	Name string
	Args []string
}

// fakeExecutor is the CommandExecutor seam test double. fn returns the
// canned output/error per call; every invocation is appended to calls so a
// test can assert on the exact command surface (the user-visible launch
// contract), not internal state.
type fakeExecutor struct {
	fn    func(name string, args []string) ([]byte, error)
	calls []recordedCall
}

func (f *fakeExecutor) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, recordedCall{Name: name, Args: append([]string(nil), args...)})
	if f.fn == nil {
		return nil, nil
	}
	return f.fn(name, args)
}

func (f *fakeExecutor) Start(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, recordedCall{Name: name, Args: append([]string(nil), args...)})
	return nil
}

// withStatDevice swaps the package statDevice seam to a fixture set: every
// path in `present` reports exists; everything else reports not-exist.
func withStatDevice(t *testing.T, present ...string) {
	t.Helper()
	set := make(map[string]struct{}, len(present))
	for _, p := range present {
		set[p] = struct{}{}
	}
	orig := statDevice
	statDevice = func(name string) (os.FileInfo, error) {
		if _, ok := set[name]; ok {
			return nil, nil
		}
		return nil, errors.New("no such file or directory")
	}
	t.Cleanup(func() { statDevice = orig })
}

func TestNewCuttlefish_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing runtime binary",
			cfg:     Config{Image: "cuttlefish:latest", Privileged: true},
			wantErr: "RuntimeBinary is required",
		},
		{
			name:    "missing image",
			cfg:     Config{RuntimeBinary: "podman", Privileged: true},
			wantErr: "Image is required",
		},
		{
			name:    "privileged disabled is rejected",
			cfg:     Config{RuntimeBinary: "podman", Image: "cuttlefish:latest", Privileged: false},
			wantErr: "Config.Privileged must be true",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCuttlefish(tc.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNewCuttlefish_DefaultsApplied(t *testing.T) {
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman",
		Image:         "cuttlefish:latest",
		Privileged:    true,
		NetworkHost:   true,
	})
	require.NoError(t, err)
	assert.Equal(t, DefaultCuttlefishTarget, c.cfg.Target)
	assert.Equal(t, "default", c.cfg.InstanceName)
	assert.Equal(t, DefaultDevices(), c.cfg.Devices)
	assert.Equal(t, DefaultGroups(), c.cfg.Groups)
	assert.Equal(t, DefaultADBSerial, c.cfg.ADBSerial)
	assert.Equal(t, "adb", c.cfg.ADBBinaryPath)
	assert.Equal(t, defaultStopGracePeriod, c.cfg.StopGracePeriod)
	assert.NotNil(t, c.executor)
}

func TestBuildContainerRunArgs_Privileged(t *testing.T) {
	args := buildContainerRunArgs(
		"cvd-default", "cuttlefish:latest",
		[]string{"/dev/kvm", "/dev/vhost-vsock"},
		[]string{"kvm", "cvdnetwork"},
		true, true,
	)

	// Primary assertion: Cuttlefish MUST be launched privileged (a non-
	// privileged container cannot make the cvd-ebr bridge). This is the
	// falsifiability mutation target.
	assert.Contains(t, args, "--privileged",
		"buildContainerRunArgs MUST include --privileged for Cuttlefish; got %v", args)

	// --network host present.
	assert.True(t, argsContainAdjacent(args, "--network", "host"),
		"expected --network host; got %v", args)

	// Each present device passed via an adjacent --device <node> pair.
	assert.True(t, argsContainAdjacent(args, "--device", "/dev/kvm"))
	assert.True(t, argsContainAdjacent(args, "--device", "/dev/vhost-vsock"))

	// Each group passed via an adjacent --group-add <grp> pair.
	assert.True(t, argsContainAdjacent(args, "--group-add", "kvm"))
	assert.True(t, argsContainAdjacent(args, "--group-add", "cvdnetwork"))

	// --name + image present; image is the final arg.
	assert.True(t, argsContainAdjacent(args, "--name", "cvd-default"))
	assert.Equal(t, "cuttlefish:latest", args[len(args)-1])
}

func TestBuildContainerRunArgs_NotPrivilegedOmitsFlags(t *testing.T) {
	// Defensive: when privileged/networkHost are false the flags are
	// omitted (proves the builder is conditional, not hardcoded). The
	// lifecycle constructor rejects Privileged=false, but the pure builder
	// must still be honest about its inputs.
	args := buildContainerRunArgs("cvd-x", "img", nil, nil, false, false)
	assert.NotContains(t, args, "--privileged")
	assert.False(t, argsContainAdjacent(args, "--network", "host"))
}

func TestBuildLaunchCvdArgs(t *testing.T) {
	assert.Equal(t, []string{"launch_cvd", "--daemon"}, buildLaunchCvdArgs(0))
	assert.Equal(t,
		[]string{"launch_cvd", "--daemon", "--base_instance_num=2"},
		buildLaunchCvdArgs(2))
}

func TestBuildStopCvdArgs(t *testing.T) {
	assert.Equal(t, []string{"stop_cvd"}, buildStopCvdArgs())
}

func TestBuildContainerExecArgs(t *testing.T) {
	assert.Equal(t,
		[]string{"exec", "cvd-default", "stop_cvd"},
		buildContainerExecArgs("cvd-default", buildStopCvdArgs()))
}

func TestCuttlefish_Launch_Success(t *testing.T) {
	withStatDevice(t, DefaultDevices()...) // all nodes present
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte("container-id-123"), nil
	}}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest",
		Privileged: true, NetworkHost: true,
		InstanceName: "tier2", Executor: fe,
	})
	require.NoError(t, err)

	res, err := c.Launch(context.Background())
	require.NoError(t, err)
	assert.True(t, res.Started)
	assert.Equal(t, "cvd-tier2", res.ContainerName)
	assert.Equal(t, DefaultCuttlefishTarget, res.Target)
	assert.Empty(t, res.MissingDevices)
	assert.ElementsMatch(t, DefaultDevices(), res.PresentDevices)

	// The run command was the privileged podman run.
	require.Len(t, fe.calls, 1)
	assert.Equal(t, "podman", fe.calls[0].Name)
	assert.Contains(t, fe.calls[0].Args, "--privileged")
}

func TestCuttlefish_Launch_MissingDeviceSurfaced(t *testing.T) {
	// Host exposes only /dev/kvm — the vhost/vsock/tun nodes are absent.
	// Launch still runs (with the present subset) but MUST surface the
	// missing nodes so the caller SKIPs-with-reason per §11.4.3.
	withStatDevice(t, "/dev/kvm")
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) { return nil, nil }}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest",
		Privileged: true, NetworkHost: true, Executor: fe,
	})
	require.NoError(t, err)

	res, err := c.Launch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/kvm"}, res.PresentDevices)
	assert.ElementsMatch(t,
		[]string{"/dev/vhost-vsock", "/dev/vhost-net", "/dev/vsock", "/dev/net/tun"},
		res.MissingDevices)
	// Only the present node reached the --device flags.
	assert.True(t, argsContainAdjacent(fe.calls[0].Args, "--device", "/dev/kvm"))
	assert.False(t, argsContainAdjacent(fe.calls[0].Args, "--device", "/dev/vhost-vsock"))
}

func TestCuttlefish_Launch_RunError(t *testing.T) {
	withStatDevice(t, DefaultDevices()...)
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte("permission denied"), errors.New("exit status 126")
	}}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest",
		Privileged: true, NetworkHost: true, Executor: fe,
	})
	require.NoError(t, err)

	res, err := c.Launch(context.Background())
	require.Error(t, err)
	assert.False(t, res.Started)
	assert.Contains(t, err.Error(), "permission denied")
}

func TestCuttlefish_Status(t *testing.T) {
	tests := []struct {
		name      string
		serial    string
		adbOut    string
		wantReady bool
	}{
		{
			name:      "configured serial online",
			serial:    "0.0.0.0:6520",
			adbOut:    "List of devices attached\n0.0.0.0:6520\tdevice\n",
			wantReady: true,
		},
		{
			name:      "configured serial offline",
			serial:    "0.0.0.0:6520",
			adbOut:    "List of devices attached\n0.0.0.0:6520\toffline\n",
			wantReady: false,
		},
		{
			name:      "configured serial absent",
			serial:    "0.0.0.0:6520",
			adbOut:    "List of devices attached\n0.0.0.0:6521\tdevice\n",
			wantReady: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
				return []byte(tc.adbOut), nil
			}}
			c, err := NewCuttlefish(Config{
				RuntimeBinary: "podman", Image: "cuttlefish:latest",
				Privileged: true, ADBSerial: tc.serial, Executor: fe,
			})
			require.NoError(t, err)

			st, err := c.Status(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.wantReady, st.Ready)
			assert.NotEmpty(t, st.Raw)
		})
	}
}

func TestCuttlefish_Stop_Sequence(t *testing.T) {
	withStatDevice(t, DefaultDevices()...)
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) { return []byte("ok"), nil }}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest",
		Privileged: true, NetworkHost: true, InstanceName: "tier2", Executor: fe,
	})
	require.NoError(t, err)
	_, err = c.Launch(context.Background())
	require.NoError(t, err)

	require.NoError(t, c.Stop(context.Background()))

	// Stop issued: graceful `podman exec cvd-tier2 stop_cvd` then
	// `podman rm -f cvd-tier2`. (Cleanup's orphan reap runs the real
	// reaper, which finds no crosvm/run_cvd on the test host.)
	var sawExecStopCvd, sawRmForce bool
	for _, call := range fe.calls {
		if call.Name == "podman" && len(call.Args) >= 3 &&
			call.Args[0] == "exec" && call.Args[1] == "cvd-tier2" && call.Args[2] == "stop_cvd" {
			sawExecStopCvd = true
		}
		if call.Name == "podman" && len(call.Args) == 3 &&
			call.Args[0] == "rm" && call.Args[1] == "-f" && call.Args[2] == "cvd-tier2" {
			sawRmForce = true
		}
	}
	assert.True(t, sawExecStopCvd, "Stop MUST run `podman exec cvd-tier2 stop_cvd`; calls=%v", fe.calls)
	assert.True(t, sawRmForce, "Stop MUST run `podman rm -f cvd-tier2`; calls=%v", fe.calls)
	assert.Empty(t, c.ContainerName(), "container name cleared after Stop")
}

func TestCuttlefish_Stop_NoLaunchIsNoOp(t *testing.T) {
	fe := &fakeExecutor{}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest", Privileged: true, Executor: fe,
	})
	require.NoError(t, err)
	require.NoError(t, c.Stop(context.Background()))
	assert.Empty(t, fe.calls, "Stop before Launch must be a no-op")
}

func TestCuttlefish_WaitForReady(t *testing.T) {
	// Becomes ready on the 2nd Status call.
	withStatDevice(t, DefaultDevices()...)
	var n int
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		n++
		if n >= 2 {
			return []byte("List of devices attached\n0.0.0.0:6520\tdevice\n"), nil
		}
		return []byte("List of devices attached\n0.0.0.0:6520\toffline\n"), nil
	}}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest", Privileged: true, Executor: fe,
	})
	require.NoError(t, err)

	_, err = c.WaitForReady(context.Background(), 10*time.Second)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 2)
}

func TestCuttlefish_WaitForReady_Timeout(t *testing.T) {
	withStatDevice(t, DefaultDevices()...)
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte("List of devices attached\n0.0.0.0:6520\toffline\n"), nil
	}}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest", Privileged: true, Executor: fe,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = c.WaitForReady(ctx, 5*time.Second)
	require.Error(t, err)
}

// argsContainAdjacent reports whether args contains the adjacent pair
// (a, b).
func argsContainAdjacent(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}
