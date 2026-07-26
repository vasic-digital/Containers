//go:build !race

package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

var e2eQemuPort atomic.Int32

func init() { e2eQemuPort.Store(14444) }

// TestQE2E verifies that the real QEMU binary at /usr/bin/qemu-system-x86_64
// starts, accepts QMP connections, handles the capability negotiation, and
// responds to a query-version command correctly. Uses -machine none (no guest
// hardware) so no kernel or initrd is required.
func TestQE2E(t *testing.T) {
	qemuBin := "/usr/bin/qemu-system-x86_64"
	if testing.Short() {
		t.Skip("skipping QEMU e2e test in short mode")
	}
	if _, err := os.Stat(qemuBin); err != nil {
		t.Skipf("qemu binary not found at %s: %v", qemuBin, err)
	}

	port := int(e2eQemuPort.Add(1) - 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, qemuBin,
		"-machine", "none",
		"-nographic",
		"-qmp", fmt.Sprintf("tcp:127.0.0.1:%d,server,nowait", port),
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start QEMU: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})

	if !waitForQMP(ctx, t, port) {
		t.Fatal("QMP socket did not become ready")
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("failed to dial QMP socket: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	greeting, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read QMP greeting: %v", err)
	}
	if !qmpValidJSONLine(t, greeting, "greeting") {
		t.Fatal("QMP greeting is not valid JSON")
	}

	if _, err := fmt.Fprintln(conn, `{"execute":"qmp_capabilities"}`); err != nil {
		t.Fatalf("failed to send qmp_capabilities: %v", err)
	}
	capResp, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read qmp_capabilities response: %v", err)
	}
	if !qmpResponseOK(t, capResp, "qmp_capabilities") {
		t.Fatal("qmp_capabilities did not return success")
	}

	if _, err := fmt.Fprintln(conn, `{"execute":"query-version"}`); err != nil {
		t.Fatalf("failed to send query-version: %v", err)
	}
	verResp, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read query-version response: %v", err)
	}
	if !qmpResponseOK(t, verResp, "query-version") {
		t.Fatal("query-version did not return success")
	}

	var ver struct {
		Return struct {
			QEMU struct {
				Major int `json:"major"`
				Minor int `json:"minor"`
				Micro int `json:"micro"`
			} `json:"qemu"`
			Package string `json:"package"`
		} `json:"return"`
	}
	if err := json.Unmarshal([]byte(verResp), &ver); err != nil {
		t.Fatalf("failed to parse query-version JSON: %v", err)
	}
	if ver.Return.QEMU.Major == 0 && ver.Return.QEMU.Minor == 0 {
		t.Fatalf("query-version returned unexpected zero version: %+v", ver.Return.QEMU)
	}
	t.Logf("QEMU version: %d.%d.%d (%s)", ver.Return.QEMU.Major, ver.Return.QEMU.Minor, ver.Return.QEMU.Micro, ver.Return.Package)

	if _, err := fmt.Fprintln(conn, `{"execute":"quit"}`); err != nil {
		t.Fatalf("failed to send quit: %v", err)
	}

	_ = cmd.Wait()
	t.Log("QEMU e2e test passed")
}

func waitForQMP(ctx context.Context, t *testing.T, port int) bool {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 1*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return false
}

func qmpResponseOK(t *testing.T, line, label string) bool {
	t.Helper()
	var env struct {
		Return json.RawMessage `json:"return"`
		Error  json.RawMessage `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Errorf("%s: not valid JSON: %v", label, err)
		return false
	}
	if env.Error != nil {
		t.Errorf("%s: QMP error: %s", label, string(env.Error))
		return false
	}
	if env.Return == nil {
		t.Errorf("%s: QMP response has no return value", label)
		return false
	}
	return true
}

func qmpValidJSONLine(t *testing.T, line, label string) bool {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Errorf("%s: not valid JSON: %v", label, err)
		return false
	}
	return true
}
