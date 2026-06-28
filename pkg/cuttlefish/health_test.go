package cuttlefish

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseADBDevices(t *testing.T) {
	out := "List of devices attached\n" +
		"0.0.0.0:6520\tdevice\n" +
		"emulator-5554\toffline\n" +
		"\n" // trailing blank line
	devices := ParseADBDevices([]byte(out))
	require.Len(t, devices, 2)
	assert.Equal(t, "0.0.0.0:6520", devices[0].Serial)
	assert.Equal(t, "device", devices[0].State)
	assert.Equal(t, "emulator-5554", devices[1].Serial)
	assert.Equal(t, "offline", devices[1].State)
}

func TestParseADBDevices_HeaderAndEmptyOnly(t *testing.T) {
	devices := ParseADBDevices([]byte("List of devices attached\n\n"))
	assert.Empty(t, devices, "header + blank lines yield no devices")
}

// TestParseADBDevices_OfflineNotOnline is the falsifiability target for the
// Online() classification: an "offline" device is NOT online.
func TestParseADBDevices_OfflineNotOnline(t *testing.T) {
	devices := ParseADBDevices([]byte("List of devices attached\n0.0.0.0:6520\toffline\n"))
	require.Len(t, devices, 1)
	assert.False(t, devices[0].Online(),
		"an offline device MUST NOT be reported online (else readiness is a §11.4 PASS-bluff)")
}

func TestADBDevice_Online(t *testing.T) {
	assert.True(t, ADBDevice{State: "device"}.Online())
	assert.False(t, ADBDevice{State: "offline"}.Online())
	assert.False(t, ADBDevice{State: "unauthorized"}.Online())
	assert.False(t, ADBDevice{State: "bootloader"}.Online())
}

func TestReadinessForSerial(t *testing.T) {
	devices := []ADBDevice{
		{Serial: "0.0.0.0:6520", State: "device"},
		{Serial: "0.0.0.0:6521", State: "offline"},
	}
	tests := []struct {
		name   string
		serial string
		want   bool
	}{
		{"named serial online", "0.0.0.0:6520", true},
		{"named serial offline", "0.0.0.0:6521", false},
		{"named serial absent", "0.0.0.0:9999", false},
		{"empty serial any-online", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, readinessForSerial(devices, tc.serial))
		})
	}
}

func TestReadinessForSerial_EmptySerialAllOffline(t *testing.T) {
	devices := []ADBDevice{
		{Serial: "0.0.0.0:6520", State: "offline"},
		{Serial: "0.0.0.0:6521", State: "unauthorized"},
	}
	assert.False(t, readinessForSerial(devices, ""),
		"empty serial must require at least one ONLINE device, not merely a listed one")
}
