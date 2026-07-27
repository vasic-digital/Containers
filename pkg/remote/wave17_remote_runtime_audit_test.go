package remote

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/runtime"
)

// TestWave17_RemoteRuntime_EscapesContainerID proves every RemoteRuntime method
// shell-escapes the caller-supplied container id before splicing it into the
// remote command string that the remote login shell interprets. Without escaping
// an id like "foo; touch /tmp/pwned" injects a second command running with the
// SSH user's privileges. RED on the pre-fix source (bare `id` → no quotes →
// Contains(escaped) fails); the polarity harness reverts individual escape sites.
func TestWave17_RemoteRuntime_EscapesContainerID(t *testing.T) {
	const malicious = "foo; touch /tmp/pwned"
	escaped := shellEscape(malicious) // 'foo; touch /tmp/pwned'
	require.NotEqual(t, malicious, escaped, "sanity: the malicious id must require escaping")

	host := RemoteHost{Name: "h", Address: "10.0.0.1", User: "deploy", Runtime: "docker"}

	var captured string
	exec := &mockExecutor{
		executeFunc: func(_ context.Context, _ RemoteHost, cmd string) (*CommandResult, error) {
			captured = cmd
			// Well-formed 7-field status line so parse steps don't error before we assert.
			return &CommandResult{
				Stdout:   "id|/name|running|healthy|2024-01-01T00:00:00Z|2024-01-01T00:00:00Z|0",
				ExitCode: 0,
			}, nil
		},
	}
	rt := NewRemoteRuntime(host, exec, logging.NopLogger{})
	ctx := context.Background()

	cases := []struct {
		name string
		call func()
	}{
		{"Start", func() { _ = rt.Start(ctx, malicious) }},
		{"Stop", func() { _ = rt.Stop(ctx, malicious) }},
		{"Remove", func() { _ = rt.Remove(ctx, malicious) }},
		{"Status", func() { _, _ = rt.Status(ctx, malicious) }},
		{"Stats", func() { _, _ = rt.Stats(ctx, malicious) }},
		{"Exec", func() { _, _ = rt.Exec(ctx, malicious, []string{"echo", "hi"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured = ""
			tc.call()
			require.Contains(t, captured, escaped,
				"%s must shell-escape the container id — got unescaped command %q (injection vector)",
				tc.name, captured)
		})
	}

	// Logs streams via ExecuteStream, a separate path.
	t.Run("Logs", func(t *testing.T) {
		var streamCmd string
		exec.executeStreamFunc = func(_ context.Context, _ RemoteHost, cmd string) (io.ReadCloser, error) {
			streamCmd = cmd
			return io.NopCloser(strings.NewReader("")), nil
		}
		_, _ = rt.Logs(ctx, malicious)
		require.Contains(t, streamCmd, escaped,
			"Logs must shell-escape the container id — got %q", streamCmd)
	})
}

// TestWave17_RemoteRuntime_EscapesListFilters proves List shell-escapes EVERY
// filter value it interpolates into the remote `ps` command — label key, label
// value, name, AND status. Each is a distinct escape site added by this batch;
// a bare (unescaped) value would let e.g. Labels{"app": "x; rm -rf /"} inject a
// second command into the remote login shell. RED on the pre-fix source (bare
// value → Contains(escaped) fails); the polarity harness reverts each site
// individually (MR3 name, MR4 label value, MR5 status, MR6 label key).
func TestWave17_RemoteRuntime_EscapesListFilters(t *testing.T) {
	host := RemoteHost{Name: "h", Address: "10.0.0.1", User: "deploy", Runtime: "docker"}

	newRT := func() (*RemoteRuntime, *string) {
		captured := new(string)
		exec := &mockExecutor{
			executeFunc: func(_ context.Context, _ RemoteHost, cmd string) (*CommandResult, error) {
				*captured = cmd
				return &CommandResult{Stdout: "", ExitCode: 0}, nil
			},
		}
		return NewRemoteRuntime(host, exec, logging.NopLogger{}), captured
	}

	const inj = "x; rm -rf /"
	escaped := shellEscape(inj)
	require.NotEqual(t, inj, escaped, "sanity: the injection value must require escaping")

	cases := []struct {
		name   string
		filter runtime.ListFilter
	}{
		{"Names", runtime.ListFilter{Names: []string{inj}}},
		{"LabelValue", runtime.ListFilter{Labels: map[string]string{"app": inj}}},
		{"LabelKey", runtime.ListFilter{Labels: map[string]string{inj: "v"}}},
		{"Status", runtime.ListFilter{Status: []runtime.ContainerState{runtime.ContainerState(inj)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, captured := newRT()
			_, _ = rt.List(context.Background(), tc.filter)
			require.Contains(t, *captured, escaped,
				"List must shell-escape the %s filter — got unescaped command %q (injection vector)",
				tc.name, *captured)
		})
	}
}
