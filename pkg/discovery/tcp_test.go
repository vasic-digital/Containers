package discovery_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"digital.vasic.containers/pkg/discovery"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTCPDiscoverer_Discover_Success(t *testing.T) {
	// Start a local TCP listener to simulate a reachable service.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	d := discovery.NewTCPDiscoverer()
	ctx := context.Background()
	target := discovery.DiscoveryTarget{
		Name:    "local-svc",
		Host:    "127.0.0.1",
		Port:    port,
		Method:  "tcp",
		Timeout: 2 * time.Second,
	}

	found, discErr := d.Discover(ctx, target)
	require.NoError(t, discErr)
	assert.True(t, found)
}

func TestTCPDiscoverer_Discover_Unreachable(t *testing.T) {
	d := discovery.NewTCPDiscoverer()
	ctx := context.Background()
	target := discovery.DiscoveryTarget{
		Name:    "absent-svc",
		Host:    "127.0.0.1",
		Port:    "1", // unlikely to be listening
		Method:  "tcp",
		Timeout: 500 * time.Millisecond,
	}

	found, err := d.Discover(ctx, target)
	assert.False(t, found)
	assert.Error(t, err)
}

func TestTCPDiscoverer_Discover_MissingHost(t *testing.T) {
	d := discovery.NewTCPDiscoverer()
	ctx := context.Background()
	target := discovery.DiscoveryTarget{
		Name: "no-host",
		Port: "8080",
	}

	found, err := d.Discover(ctx, target)
	assert.False(t, found)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "host and port are required")
}

func TestTCPDiscoverer_Discover_MissingPort(t *testing.T) {
	d := discovery.NewTCPDiscoverer()
	ctx := context.Background()
	target := discovery.DiscoveryTarget{
		Name: "no-port",
		Host: "127.0.0.1",
	}

	found, err := d.Discover(ctx, target)
	assert.False(t, found)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "host and port are required")
}

func TestTCPDiscoverer_Discover_CancelledContext(t *testing.T) {
	d := discovery.NewTCPDiscoverer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	target := discovery.DiscoveryTarget{
		Name:    "cancelled",
		Host:    "127.0.0.1",
		Port:    "9999",
		Timeout: 5 * time.Second,
	}

	found, err := d.Discover(ctx, target)
	assert.False(t, found)
	assert.Error(t, err)
}

// TestTCPDiscoverer_Discover_HandshakeOnly_Characterization documents the
// deliberately weak contract stated on TCPDiscoverer.Discover: a listener
// that NEVER calls Accept still reports reachable, because the kernel
// completes the TCP handshake from the listen backlog with no application
// bytes exchanged.
//
// This is a CHARACTERIZATION / CONTRACT test recording the CURRENT intended
// behaviour (§11.4.108 doc-honesty) — it is NOT a §11.4.115 RED->GREEN defect
// guard: "handshake-only" is the designed contract, not a bug, so there is no
// broken state to reproduce and flip to green.
func TestTCPDiscoverer_Discover_HandshakeOnly_Characterization(t *testing.T) {
	// Open a listener but NEVER call Accept; inbound connections sit in the
	// kernel accept backlog. A dial still completes the handshake.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	d := discovery.NewTCPDiscoverer()
	target := discovery.DiscoveryTarget{
		Name:    "handshake-only",
		Host:    "127.0.0.1",
		Port:    port,
		Method:  "tcp",
		Timeout: 2 * time.Second,
	}

	found, discErr := d.Discover(context.Background(), target)
	// Reachable == handshake completed, NOT that any service served bytes.
	require.NoError(t, discErr)
	assert.True(t, found)
}

// TestTCPDiscoverer_Discover_DistinguishesIndeterminateFromDefinitiveRefusal
// is the §11.4.115 regression guard for DISC-3: a bare found==false
// conflates a definitively-refused connection with an indeterminate probe
// outcome (context cancel). Discover's doc comment promises this IS
// distinguishable by inspecting the wrapped error's class
// (errors.Is(err, context.Canceled), a net.Error.Timeout() assertion) — this
// test proves that promise holds using two fully deterministic, no-mock,
// loopback-only scenarios.
//
// §11.4.115 surgical-revert evidence: changing the trailing "%w" verb to
// "%v" in TCPDiscoverer.Discover's dial-error wrap (tcp.go) breaks the
// Unwrap chain these assertions depend on. That revert was performed,
// confirmed `--- FAIL` on this test, and reverted back to "%w" — see the
// DISC-3 finding note in the containers pkg/discovery work log for the
// captured command + output.
func TestTCPDiscoverer_Discover_DistinguishesIndeterminateFromDefinitiveRefusal(t *testing.T) {
	d := discovery.NewTCPDiscoverer()

	t.Run("cancelled context is indeterminate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		target := discovery.DiscoveryTarget{
			Name:    "cancelled-indeterminate",
			Host:    "127.0.0.1",
			Port:    "9",
			Timeout: 2 * time.Second,
		}

		found, err := d.Discover(ctx, target)
		assert.False(t, found)
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled),
			"a cancelled probe MUST be classifiable as indeterminate via errors.Is(err, context.Canceled)")
	})

	t.Run("closed local port is a definitive refusal, not indeterminate", func(t *testing.T) {
		// Reserve an ephemeral port, then close the listener immediately so
		// nothing is listening. Dialing loopback with nobody accepting
		// deterministically yields a connection-refused RST on Linux — no
		// network dependency, no timing race.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		_, port, err := net.SplitHostPort(ln.Addr().String())
		require.NoError(t, err)
		require.NoError(t, ln.Close())

		target := discovery.DiscoveryTarget{
			Name:    "refused-definitive",
			Host:    "127.0.0.1",
			Port:    port,
			Timeout: 2 * time.Second,
		}

		found, discErr := d.Discover(context.Background(), target)
		assert.False(t, found)
		require.Error(t, discErr)
		assert.False(t, errors.Is(discErr, context.Canceled),
			"a definitive connection-refused MUST NOT be misclassified as the cancelled-context indeterminate case")

		var netErr net.Error
		require.True(t, errors.As(discErr, &netErr), "wrapped cause must unwrap to a net.Error")
		assert.False(t, netErr.Timeout(),
			"a definitive connection-refused MUST NOT report Timeout()==true (that signature is reserved for the indeterminate case)")
	})
}

// TestTCPDiscoverer_Discover_ZeroTimeout tests that a zero timeout
// uses the default 5 second timeout.
func TestTCPDiscoverer_Discover_ZeroTimeout(t *testing.T) {
	// Start a local TCP listener to simulate a reachable service.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	d := discovery.NewTCPDiscoverer()
	ctx := context.Background()
	target := discovery.DiscoveryTarget{
		Name:    "zero-timeout-svc",
		Host:    "127.0.0.1",
		Port:    port,
		Method:  "tcp",
		Timeout: 0, // Explicitly zero to trigger default
	}

	found, discErr := d.Discover(ctx, target)
	require.NoError(t, discErr)
	assert.True(t, found)
}
