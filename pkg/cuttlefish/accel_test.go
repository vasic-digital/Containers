package cuttlefish

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withKVMDevicePath redirects the package-level kvmDevicePath seam to a
// caller-supplied path, restoring it on cleanup, so both the present and
// absent KVM branches are exercised without a real /dev/kvm.
func withKVMDevicePath(t *testing.T, path string) {
	t.Helper()
	orig := kvmDevicePath
	kvmDevicePath = path
	t.Cleanup(func() { kvmDevicePath = orig })
}

func TestKVMAvailable_PresentAndAbsent(t *testing.T) {
	present := t.TempDir() + "/kvm"
	require.NoError(t, os.WriteFile(present, nil, 0o644))
	withKVMDevicePath(t, present)
	assert.True(t, KVMAvailable(), "KVMAvailable must be true when the node exists")

	withKVMDevicePath(t, t.TempDir()+"/this-kvm-does-not-exist")
	assert.False(t, KVMAvailable(), "KVMAvailable must be false when the node is absent")
}

func TestRequireKVM_Present(t *testing.T) {
	present := t.TempDir() + "/kvm"
	require.NoError(t, os.WriteFile(present, nil, 0o644))
	withKVMDevicePath(t, present)
	assert.NoError(t, RequireKVM())
}

func TestRequireKVM_Absent(t *testing.T) {
	withKVMDevicePath(t, t.TempDir()+"/absent")
	err := RequireKVM()
	require.Error(t, err)
	// The error names the missing capability + the documented pre-flight,
	// so a caller SKIPs-with-reason per §11.4.3 rather than PASSing.
	assert.Contains(t, err.Error(), "requires KVM")
	assert.Contains(t, err.Error(), "find /dev -name kvm")
}

// TestRequireKVM_TopologyGate demonstrates the §11.4.3 honest SKIP pattern
// on the REAL host: where /dev/kvm is absent (e.g. macOS / Apple Silicon)
// the gate returns an error and the test SKIPs-with-reason instead of
// PASSing-by-default. Where /dev/kvm is present (a Linux + KVM host) the
// gate opens.
func TestRequireKVM_TopologyGate(t *testing.T) {
	if err := RequireKVM(); err != nil {
		t.Skipf("SKIP-OK (§11.4.3 topology): host cannot run Cuttlefish: %v", err)
	}
	// Reached only on a Linux + KVM host — the gate correctly opened.
	assert.True(t, KVMAvailable())
}

func TestFilterPresentDevices_PartialHost(t *testing.T) {
	// Host exposes only /dev/kvm + /dev/net/tun.
	withStatDevice(t, "/dev/kvm", "/dev/net/tun")
	present, missing := FilterPresentDevices(DefaultDevices())

	// Primary assertion (falsifiability target): an absent node MUST land
	// in `missing`, not `present`.
	assert.Equal(t, []string{"/dev/kvm", "/dev/net/tun"}, present)
	assert.ElementsMatch(t,
		[]string{"/dev/vhost-vsock", "/dev/vhost-net", "/dev/vsock"},
		missing)
}

func TestFilterPresentDevices_AllPresent(t *testing.T) {
	withStatDevice(t, DefaultDevices()...)
	present, missing := FilterPresentDevices(DefaultDevices())
	assert.Equal(t, DefaultDevices(), present)
	assert.Empty(t, missing)
}

func TestFilterPresentDevices_NonePresent(t *testing.T) {
	withStatDevice(t) // empty present set
	present, missing := FilterPresentDevices(DefaultDevices())
	assert.Empty(t, present)
	assert.Equal(t, DefaultDevices(), missing)
}
