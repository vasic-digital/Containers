package vm

// Wave-20 VM2-HARD permanent regression guards (§11.4.115 GREEN
// polarity). Each guard below asserts the FIXED behavior of a §11.4.118
// audit finding. The paired RED reproduction (surgical revert of the
// fix, capturing the real `--- FAIL` line, then restore) is recorded in
// the guard's own doc comment rather than committed as a live test —
// a deliberately-hanging or deliberately-broken test must never live in
// the permanent suite (mirrors the wave19 CT-HARDEN-VM-1 convention:
// "gated deadlock-on-fix RED reproduction was evidence-only and is not
// committed").
//
//   - VM2-1 (clients.go realQMPClient — Dial/SystemPowerdown/Screendump
//     never bounded any post-connect conn read/write by a deadline or
//     ctx cancellation): guarded by
//     TestRealQMPClient_Dial_BoundedByDeadlineWhenPeerNeverSpeaks. RED
//     reproduction: with `conn.SetDeadline(qmpDeadline(ctx))` removed
//     from Dial, the guard's own 5s watchdog fires and the test FAILs
//     with "VM2-1 REGRESSION: Dial hung past the watchdog window
//     against a peer that never speaks — the QMP conn deadline is not
//     being enforced" (observed `--- FAIL:
//     TestRealQMPClient_Dial_BoundedByDeadlineWhenPeerNeverSpeaks` at
//     ~5.00s during the authoring of this fix). Restored immediately
//     after capturing that line.
//   - VM2-3 (clients.go realQMPClient.Screendump — single-line
//     strings.Contains(resp, `"error"`) misreads an async {"event":...}
//     line landing between the command and its response): guarded by
//     TestRealQMPClient_Screendump_SkipsAsyncEventBeforeRealError +
//     TestRealQMPClient_Screendump_DoesNotFalsePositiveOnEventPayload-
//     ContainingErrorSubstring. RED reproduction: with Screendump
//     reverted to a single `r.reader.ReadString('\n')` +
//     `strings.Contains(resp, "\"error\"")` check, BOTH guards FAIL:
//     the first with "Screendump: want error ... got nil — false PASS"
//     (the async `{"event":"STOP",...}` line is read as THE response
//     and contains no literal `"error"` substring, so the reverted code
//     returns nil while the real {"error":...} the guest actually sent
//     is left unread on the wire); the second with "Screendump: want
//     nil ... got: realQMPClient.Screendump: qemu rejected:
//     {"event":"DEVICE_TRAY_MOVED","data":{"reason":"error"}}" (a
//     benign event whose payload happens to contain the literal
//     substring `"error"` is misread as a rejection of a screendump
//     that actually succeeded) — observed `--- FAIL:
//     TestRealQMPClient_Screendump_SkipsAsyncEventBeforeRealError` and
//     `--- FAIL:
//     TestRealQMPClient_Screendump_DoesNotFalsePositiveOnEventPayload-
//     ContainingErrorSubstring`. Restored immediately after capturing
//     both lines.
//   - VM2-5 (clients.go realSSHClient.Download — no `size < 0` guard
//     before io.CopyN, so a garbled/negative-size SCP header produces a
//     "clean" 0-byte file recorded upstream as captured evidence):
//     guarded by TestRealSSHClient_Download_RejectsNegativeSizeHeader
//     (fixture completes the full SCP sink handshake normally, as a
//     real garbled/malicious peer would, so the pre-fix code path is
//     genuinely exercised end-to-end rather than desyncing on an
//     early channel close). RED reproduction: with the
//     `if size < 0 { ... }` guard removed, io.CopyN(out, reader, -1)
//     returns (0, nil) and Download itself returns nil — the test FAILs
//     with "Download with negative-size header: want error, got nil" —
//     observed `--- FAIL:
//     TestRealSSHClient_Download_RejectsNegativeSizeHeader`. Restored
//     immediately after capturing that line.
//
// A bonus (not individually mandated, but cheap and load-bearing) guard
// for VM2-2 is included below — TestOsProcessRunner_StartDetached_*   —
// using a real `sh` subprocess (NOT real qemu/ssh; §11.4.27 is
// satisfied because `sh` is a generic injectable "name" the production
// processRunner interface accepts, exactly like the fakes in
// qemu_test.go accept an arbitrary name).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// -----------------------------------------------------------------------------
// VM2-1 — realQMPClient bounded by a deadline against a peer that never speaks
// -----------------------------------------------------------------------------

// TestRealQMPClient_Dial_BoundedByDeadlineWhenPeerNeverSpeaks is the
// permanent VM2-1 guard. The in-process "server" accepts the TCP
// connect (so net.DialTimeout itself succeeds) and then never writes a
// single byte — simulating a QEMU that accepted the monitor connection
// but stalled before its greeting. Pre-fix, realQMPClient.Dial's
// ReadString('\n') on that connection blocks forever; post-fix, the
// ctx-derived conn deadline (qmpDeadline) bounds it.
//
// The 5s "watchdog" select branch is the anti-hang safety net for THIS
// test itself: a regression that drops the deadline must not wedge the
// whole `go test` invocation, it must surface as an honest FAIL.
func TestRealQMPClient_Dial_BoundedByDeadlineWhenPeerNeverSpeaks(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- c
		// Deliberately never write anything — the peer stalls silently.
	}()

	// ctx carries a short deadline so the fix's qmpDeadline(ctx) path is
	// exercised (rather than the 10s qmpDefaultOpTimeout fallback), so
	// the guard runs fast in CI while still exercising the real code
	// path production uses when the matrix runner's own ctx has a
	// deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	c := &realQMPClient{}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Dial(ctx, port, 5*time.Second) }()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("Dial against a peer that never speaks: want a bounded timeout error, got nil")
		}
		if elapsed > 3*time.Second {
			t.Fatalf("Dial returned only after %s — expected bounded by the ~200ms ctx deadline, not a multi-second stall", elapsed)
		}
		if !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("Dial error: want a timeout-class error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("VM2-1 REGRESSION: Dial hung past the watchdog window against a peer that never speaks — the QMP conn deadline is not being enforced")
	}

	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatalf("server-side accept never completed")
	}
}

// -----------------------------------------------------------------------------
// VM2-3 — Screendump discriminates an async {"event":...} from the real response
// -----------------------------------------------------------------------------

// qmpHandshakeThenCustom spins up a TCP listener that performs the
// standard QMP greeting + qmp_capabilities negotiation (mirroring
// startQMPServer in clients_test.go) and then hands control to
// afterCapabilities to write whatever custom response sequence the test
// wants after reading the client's next command.
func qmpHandshakeThenCustom(t *testing.T, afterCapabilities func(conn net.Conn, reader *bufio.Reader)) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, `{"QMP":{"version":{"qemu":{"major":8,"minor":0}},"capabilities":[]}}`+"\n")
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil { // qmp_capabilities request
			return
		}
		_, _ = io.WriteString(conn, `{"return":{}}`+"\n")
		if _, err := reader.ReadString('\n'); err != nil { // the screendump command
			return
		}
		afterCapabilities(conn, reader)
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return port
}

// TestRealQMPClient_Screendump_SkipsAsyncEventBeforeRealError is the
// primary permanent VM2-3 guard (false-PASS direction): an unsolicited
// {"event":...} notification lands on the wire BEFORE the guest's real
// {"error":...} response to the screendump command. A single-line,
// substring-based reader mis-reads the event line as THE response (no
// "error" substring in it) and returns nil — a false PASS while the
// guest actually rejected the screendump. The fixed reader must skip
// the event and surface the real error.
func TestRealQMPClient_Screendump_SkipsAsyncEventBeforeRealError(t *testing.T) {
	port := qmpHandshakeThenCustom(t, func(conn net.Conn, _ *bufio.Reader) {
		_, _ = io.WriteString(conn, `{"event":"STOP","data":{},"timestamp":{"seconds":1,"microseconds":0}}`+"\n")
		_, _ = io.WriteString(conn, `{"error":{"class":"GenericError","desc":"boom"}}`+"\n")
	})
	c := &realQMPClient{}
	if err := c.Dial(t.Context(), port, 5*time.Second); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	err := c.Screendump(t.Context(), "/tmp/whatever-vm2-3.ppm")
	if err == nil {
		t.Fatalf("Screendump: want error (qemu genuinely rejected after an async event), got nil — false PASS")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Screendump error: want it to surface the real {\"error\":...} payload (\"boom\"), got: %v", err)
	}
}

// TestRealQMPClient_Screendump_DoesNotFalsePositiveOnEventPayloadContainingErrorSubstring
// is the complementary permanent VM2-3 guard (false-FAIL direction): an
// async event whose own JSON payload happens to contain the substring
// "error" (e.g. a device-name or log field) lands before the real
// {"return":{}} success response. The pre-fix substring check would
// have flagged this as a rejection; the fixed JSON-envelope reader must
// recognise the event line as non-terminal and read through to the real
// {"return":{}}.
func TestRealQMPClient_Screendump_DoesNotFalsePositiveOnEventPayloadContainingErrorSubstring(t *testing.T) {
	port := qmpHandshakeThenCustom(t, func(conn net.Conn, _ *bufio.Reader) {
		// The literal quoted substring `"error"` (not merely the bare
		// word "error") is exactly what the pre-fix
		// strings.Contains(resp, `"error"`) check matched on — this
		// payload is deliberately chosen to trigger that substring.
		_, _ = io.WriteString(conn, `{"event":"DEVICE_TRAY_MOVED","data":{"reason":"error"}}`+"\n")
		_, _ = io.WriteString(conn, `{"return":{}}`+"\n")
	})
	c := &realQMPClient{}
	if err := c.Dial(t.Context(), port, 5*time.Second); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Screendump(t.Context(), "/tmp/whatever-vm2-3b.ppm"); err != nil {
		t.Fatalf("Screendump: want nil (genuine success after a benign event whose payload contains \"error\" as a substring), got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// VM2-5 — Download rejects a negative-size SCP header before touching the file
// -----------------------------------------------------------------------------

// TestRealSSHClient_Download_RejectsNegativeSizeHeader is the permanent
// VM2-5 guard. The in-process SCP-sink fixture sends the garbled header
// `C0644 -1 evidence.ppm` (a negative size). Pre-fix,
// io.CopyN(out, reader, -1) returns (0, nil) immediately — a "clean"
// 0-byte file that matrix.go's Download-err==nil captor would have
// recorded as genuine captured evidence. Post-fix, Download must reject
// BEFORE ever creating the destination file.
func TestRealSSHClient_Download_RejectsNegativeSizeHeader(t *testing.T) {
	port := startSSHServer(t, sshServerOpts{
		sessionHandler: func(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) {
			cmd, ok := readExecCommand(reqs)
			if !ok || !strings.HasPrefix(cmd, "scp -f ") {
				_ = ch.Close()
				return
			}
			reader := bufio.NewReader(ch)
			if _, err := reader.ReadByte(); err != nil { // client's "ready" NUL
				return
			}
			// VM2-5 regression fixture: a garbled/negative-size header,
			// otherwise completing the SCP sink protocol NORMALLY (a
			// real malicious/garbled peer that finishes the handshake
			// rather than dropping the connection). This is what lets
			// the pre-fix vulnerability manifest as a SILENT 0-byte
			// "success" (Download returns nil) rather than a generic
			// protocol-desync error — the fixed client must reject
			// BEFORE ever reaching the ack-header write below, so on
			// the fixed path these subsequent ReadByte calls never see
			// a byte and simply block until the test's client-side
			// Close() (t.Cleanup) tears the channel down.
			_, _ = fmt.Fprint(ch, "C0644 -1 evidence.ppm\n")
			if _, err := reader.ReadByte(); err != nil { // ack-header NUL
				return
			}
			// No body bytes to send for a negative/garbled size.
			if _, err := reader.ReadByte(); err != nil { // ack-terminator NUL
				return
			}
			sendExitStatus(ch, 0)
		},
	})

	c := connectAuthenticated(t, port, "")
	dst := filepath.Join(t.TempDir(), "evidence.ppm")
	err := c.Download(t.Context(), "/guest/evidence.ppm", dst)
	if err == nil {
		t.Fatalf("Download with negative-size header: want error, got nil")
	}
	if !strings.Contains(err.Error(), "negative size") {
		t.Fatalf("Download error: want it to mention the negative size, got: %v", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Fatalf("Download must NOT create %s on a rejected negative-size header (0-byte false-PASS evidence file)", dst)
	}
}

// -----------------------------------------------------------------------------
// Bonus — VM2-2 dead-on-arrival diagnostics (not individually mandated, but
// cheap + load-bearing; uses a real `sh` subprocess, never real qemu).
// -----------------------------------------------------------------------------

// TestOsProcessRunner_StartDetached_SurfacesStderrOnImmediateCrash proves
// the VM2-2 fix: a process that exits within the liveness window returns
// an error citing its captured stderr, instead of a bare Start()-only
// error (the pre-fix behaviour discarded Stdout/Stderr entirely and let
// the caller burn the full BootTimeout with no diagnostic at all).
func TestOsProcessRunner_StartDetached_SurfacesStderrOnImmediateCrash(t *testing.T) {
	prevWindow := livenessCheckWindow
	livenessCheckWindow = 500 * time.Millisecond
	defer func() { livenessCheckWindow = prevWindow }()

	r := osProcessRunner{}
	err := r.StartDetached("sh", "-c", "echo 'boom: bad QCowPath' 1>&2; exit 1")
	if err == nil {
		t.Fatalf("StartDetached for an immediately-crashing process: want a diagnostic error, got nil")
	}
	if !strings.Contains(err.Error(), "boom: bad QCowPath") {
		t.Fatalf("StartDetached error: want it to surface the captured stderr, got: %v", err)
	}
}

// TestOsProcessRunner_StartDetached_SucceedsForLongRunningProcess proves
// the liveness check does not regress the normal (genuinely-booting)
// path: a process that outlives the liveness window still returns nil
// promptly (bounded by livenessCheckWindow, not by the process's own
// lifetime).
func TestOsProcessRunner_StartDetached_SucceedsForLongRunningProcess(t *testing.T) {
	prevWindow := livenessCheckWindow
	livenessCheckWindow = 200 * time.Millisecond
	defer func() { livenessCheckWindow = prevWindow }()

	r := osProcessRunner{}
	start := time.Now()
	err := r.StartDetached("sh", "-c", "sleep 5")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("StartDetached for a long-running process: want nil, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("StartDetached took %s — want it bounded by ~%s (the liveness window), not the process's own 5s lifetime", elapsed, livenessCheckWindow)
	}
}
