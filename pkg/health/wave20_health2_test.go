package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the permanent §11.4.115 GREEN-polarity guard suite for the
// Wave-20 DEEPER (§11.4.118) pkg/health 2nd-pass hardening batch (HEALTH2-N).
// Every test asserts the FIXED behavior; each finding was reproduced first
// (RED) on the pre-fix code by surgically reverting the single-line fix anchor
// before the fix landed — see the conductor report for the captured RED
// evidence.

// ---------------------------------------------------------------------
// HEALTH2-1: HelixServiceHealthChecker.checkOnce must treat an unset (<=0)
// Timeout as "use a package default", exactly like every sibling checker
// (CheckTCP / CheckHTTP / DefaultChecker.Check). Pre-fix, checkOnce fed the
// raw Timeout straight into context.WithTimeout(ctx, 0), which is
// immediately-expired, so a check against a LIVE, open target failed with a
// false "deadline exceeded" and a genuinely-healthy service was reported
// UNHEALTHY — a §11.4.1 FAIL-bluff (false negative). The HelixServiceHealth-
// Checker struct + fields are exported, so a consumer constructing one
// without setting Timeout (the zero value) is a realistic path.
//
// Load-bearing oracle: a REAL listener / REAL httptest server that any
// positive timeout certifies as healthy. Only the missing timeout<=0->default
// clamp can flip it to unhealthy.
// ---------------------------------------------------------------------

func TestWave20_HEALTH2_CheckOnceZeroTimeoutTCP_UsesDefaultNotInstantFail(t *testing.T) {
	// A live TCP listener. The kernel completes the handshake from its
	// backlog even without an explicit Accept(), so a dial to it succeeds
	// under any positive timeout.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port)
	require.NoError(t, err)

	h := &HelixServiceHealthChecker{
		ServiceName: "health2-zero-timeout-tcp",
		CheckType:   "tcp",
		Host:        host,
		Port:        portNum,
		Timeout:     0, // unset — the exported struct's zero value
		Retries:     0,
	}

	status, err := h.Check(context.Background())

	require.NoError(t, err,
		"an unset (0) Timeout must fall back to a default, not error against a live open port")
	assert.True(t, status.Healthy,
		"checkOnce with Timeout<=0 must use a default timeout — an immediately-expired "+
			"context.WithTimeout(ctx, 0) reports a healthy live TCP port as UNHEALTHY")
}

func TestWave20_HEALTH2_CheckOnceZeroTimeoutHTTP_UsesDefaultNotInstantFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	))
	defer srv.Close()

	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port)
	require.NoError(t, err)

	h := &HelixServiceHealthChecker{
		ServiceName: "health2-zero-timeout-http",
		CheckType:   "http",
		Host:        host,
		Port:        portNum,
		Path:        "/",
		Timeout:     0, // unset
		Retries:     0,
	}

	status, err := h.Check(context.Background())

	require.NoError(t, err,
		"an unset (0) Timeout must fall back to a default, not error against a live 200 endpoint")
	assert.True(t, status.Healthy,
		"checkOnce (http) with Timeout<=0 must use a default timeout — an immediately-expired "+
			"context.WithTimeout(ctx, 0) reports a healthy live HTTP endpoint as UNHEALTHY")
}

// TestWave20_HEALTH2_CheckOncePositiveTimeout_NegativeControl is the negative
// control: a POSITIVE Timeout must keep its own value (the clamp must not
// override a real, caller-supplied timeout) and still certify a live target
// healthy — proving the fix touches only the unset (<=0) path.
func TestWave20_HEALTH2_CheckOncePositiveTimeout_NegativeControl(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port)
	require.NoError(t, err)

	h := &HelixServiceHealthChecker{
		ServiceName: "health2-positive-timeout-tcp",
		CheckType:   "tcp",
		Host:        host,
		Port:        portNum,
		Timeout:     2000000000, // 2s, positive — must be honored unchanged
		Retries:     0,
	}

	status, err := h.Check(context.Background())
	require.NoError(t, err)
	assert.True(t, status.Healthy)
}
