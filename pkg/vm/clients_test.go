package vm

import (
	"bufio"
	"bytes"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// -----------------------------------------------------------------------------
// In-process SSH server harness
// -----------------------------------------------------------------------------
//
// Anti-bluff posture: these tests drive the real realSSHClient code path
// against a real loopback SSH server using golang.org/x/crypto/ssh's server
// primitives. The "fakes" here ARE real SSH servers — they speak the same
// protocol the production server does. The thing being faked is the guest
// OS state machine on top of the SSH session (a handler that simulates
// what `scp -t` or a shell would do), NOT the SSH layer itself.

// sshServerOpts configures the in-process SSH server for one test.
type sshServerOpts struct {
	// sessionHandler is invoked once per "session" channel opened by the
	// client. The handler reads the channel's exec command (via the
	// ssh.Request stream), simulates the requested operation, and closes
	// the channel with an explicit exit-status reply.
	sessionHandler func(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request)
	// authorizedKey, when non-nil, switches the server to public-key auth
	// and accepts ONLY this key. When nil the server accepts any password.
	authorizedKey ssh.PublicKey
}

// startSSHServer spins up a loopback SSH server and returns its listener
// port. The listener is closed via t.Cleanup.
func startSSHServer(t *testing.T, opts sshServerOpts) int {
	t.Helper()

	rsaKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatalf("startSSHServer: rsa.GenerateKey: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("startSSHServer: NewSignerFromKey: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startSSHServer: listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	cfg := &ssh.ServerConfig{}
	if opts.authorizedKey != nil {
		expectedMarshalled := opts.authorizedKey.Marshal()
		cfg.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), expectedMarshalled) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("public key not authorized")
		}
	} else {
		cfg.PasswordCallback = func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		}
	}
	cfg.AddHostKey(hostSigner)

	go func() {
		for {
			tcpConn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					_ = c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						_ = newCh.Reject(ssh.UnknownChannelType, "")
						continue
					}
					ch, requests, err := newCh.Accept()
					if err != nil {
						return
					}
					go func() {
						if opts.sessionHandler != nil {
							opts.sessionHandler(t, ch, requests)
						} else {
							_ = ch.Close()
						}
					}()
				}
			}(tcpConn)
		}
	}()

	t.Cleanup(func() { _ = listener.Close() })
	return port
}

// readExecCommand pulls the "exec" request payload out of a session's
// request stream. Returns the requested command and acknowledges the
// request with a positive reply.
func readExecCommand(reqs <-chan *ssh.Request) (string, bool) {
	for req := range reqs {
		switch req.Type {
		case "exec":
			cmd := string(req.Payload[4:]) // first 4 bytes are length
			_ = req.Reply(true, nil)
			return cmd, true
		case "shell":
			_ = req.Reply(true, nil)
			return "", true
		case "env":
			_ = req.Reply(true, nil)
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
	return "", false
}

// sendExitStatus closes the channel with an explicit exit-status reply
// (the same protocol level OpenSSH's sshd uses).
func sendExitStatus(ch ssh.Channel, code uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
	_ = ch.Close()
}

// connectAuthenticated dials the realSSHClient against an in-process server
// and returns the authenticated client. Used by Run/Upload/Download tests.
func connectAuthenticated(t *testing.T, port int, keyPath string) *realSSHClient {
	t.Helper()
	c := newRealSSHClient("testuser", keyPath)
	if err := c.Authenticate(t.Context(), port, 5*time.Second); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// writeKeyPairToTmp generates an ephemeral RSA keypair, writes the private
// key (PEM) to a tmp file, and returns (privateKeyPath, publicKey).
func writeKeyPairToTmp(t *testing.T, dir string) (string, ssh.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	keyPath := filepath.Join(dir, "id_rsa")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	return keyPath, pub
}

// -----------------------------------------------------------------------------
// Tests — realSSHClient.Run
// -----------------------------------------------------------------------------

func TestRealSSHClient_Run_CapturesStdoutStderrAndExitCode(t *testing.T) {
	port := startSSHServer(t, sshServerOpts{
		sessionHandler: func(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) {
			cmd, ok := readExecCommand(reqs)
			if !ok {
				_ = ch.Close()
				return
			}
			// Simulate the script behaviour the test expects: print "hi"
			// to stdout, "err" to stderr, exit 7.
			if strings.Contains(cmd, "echo hi") {
				_, _ = io.WriteString(ch, "hi\n")
				_, _ = io.WriteString(ch.Stderr(), "err\n")
				sendExitStatus(ch, 7)
				return
			}
			sendExitStatus(ch, 0)
		},
	})
	c := connectAuthenticated(t, port, "")
	stdout, stderr, exitCode, err := c.Run(t.Context(), "echo hi; echo err >&2; exit 7", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if stdout != "hi\n" {
		t.Fatalf("stdout: want %q got %q", "hi\n", stdout)
	}
	if stderr != "err\n" {
		t.Fatalf("stderr: want %q got %q", "err\n", stderr)
	}
	if exitCode != 7 {
		t.Fatalf("exitCode: want 7 got %d", exitCode)
	}
}

// -----------------------------------------------------------------------------
// Tests — realSSHClient.Upload (SCP source protocol)
// -----------------------------------------------------------------------------

// scpReceivedFile captures what the in-process server saw on stdin during
// `scp -t <dir>`. Used to assert the client transmitted the expected bytes.
type scpReceivedFile struct {
	mode os.FileMode
	size int64
	name string
	data []byte
}

func TestRealSSHClient_Upload_TransfersFileContents(t *testing.T) {
	dir := t.TempDir()
	hostSrc := filepath.Join(dir, "host-source")
	wantBytes := []byte("hello scp upload payload\n")
	if err := os.WriteFile(hostSrc, wantBytes, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	var (
		mu       sync.Mutex
		captured *scpReceivedFile
	)
	port := startSSHServer(t, sshServerOpts{
		sessionHandler: func(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) {
			cmd, ok := readExecCommand(reqs)
			if !ok || !strings.HasPrefix(cmd, "scp -t ") {
				_ = ch.Close()
				return
			}
			reader := bufio.NewReader(ch)
			header, err := reader.ReadString('\n')
			if err != nil {
				sendExitStatus(ch, 1)
				return
			}
			var mode os.FileMode
			var size int64
			var name string
			if _, err := fmt.Sscanf(header, "C%o %d %s", &mode, &size, &name); err != nil {
				sendExitStatus(ch, 2)
				return
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(reader, body); err != nil {
				sendExitStatus(ch, 3)
				return
			}
			// Read trailing NUL terminator.
			_, _ = reader.ReadByte()
			mu.Lock()
			captured = &scpReceivedFile{mode: mode, size: size, name: name, data: body}
			mu.Unlock()
			sendExitStatus(ch, 0)
		},
	})

	c := connectAuthenticated(t, port, "")
	if err := c.Upload(t.Context(), hostSrc, "/tmp/dest/host-source"); err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatalf("server never received the file")
	}
	if captured.name != "host-source" {
		t.Fatalf("name: want host-source got %q", captured.name)
	}
	if captured.size != int64(len(wantBytes)) {
		t.Fatalf("size: want %d got %d", len(wantBytes), captured.size)
	}
	if !bytes.Equal(captured.data, wantBytes) {
		t.Fatalf("data: want %q got %q", wantBytes, captured.data)
	}
}

// -----------------------------------------------------------------------------
// Tests — realSSHClient.Download (SCP sink protocol)
// -----------------------------------------------------------------------------

func TestRealSSHClient_Download_PullsFileContents(t *testing.T) {
	wantBytes := []byte("payload from guest\n")
	port := startSSHServer(t, sshServerOpts{
		sessionHandler: func(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) {
			cmd, ok := readExecCommand(reqs)
			if !ok || !strings.HasPrefix(cmd, "scp -f ") {
				_ = ch.Close()
				return
			}
			reader := bufio.NewReader(ch)
			// Wait for client's "ready" NUL.
			if _, err := reader.ReadByte(); err != nil {
				sendExitStatus(ch, 1)
				return
			}
			// Send header.
			fmt.Fprintf(ch, "C%#o %d %s\n", os.FileMode(0o644), len(wantBytes), "guest-source")
			// Wait for ack NUL.
			if _, err := reader.ReadByte(); err != nil {
				sendExitStatus(ch, 2)
				return
			}
			_, _ = ch.Write(wantBytes)
			// Wait for final ack NUL.
			if _, err := reader.ReadByte(); err != nil {
				sendExitStatus(ch, 3)
				return
			}
			sendExitStatus(ch, 0)
		},
	})

	c := connectAuthenticated(t, port, "")
	dst := filepath.Join(t.TempDir(), "host-dest")
	if err := c.Download(t.Context(), "/guest/path/guest-source", dst); err != nil {
		t.Fatalf("Download returned error: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Fatalf("downloaded bytes: want %q got %q", wantBytes, got)
	}
}

// -----------------------------------------------------------------------------
// Tests — realSSHClient.Authenticate (key auth)
// -----------------------------------------------------------------------------

func TestRealSSHClient_Authenticate_KeyAuth_AcceptsValidKey(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := writeKeyPairToTmp(t, dir)
	port := startSSHServer(t, sshServerOpts{authorizedKey: pub})
	c := newRealSSHClient("testuser", keyPath)
	if err := c.Authenticate(t.Context(), port, 5*time.Second); err != nil {
		t.Fatalf("Authenticate with valid key: %v", err)
	}
	defer c.Close()
	if c.client == nil {
		t.Fatalf("client field nil after successful Authenticate")
	}
}

func TestRealSSHClient_Authenticate_KeyAuth_RejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	// Generate two keypairs; server accepts only key A; client uses key B.
	_, pubA := writeKeyPairToTmp(t, dir)
	dirB := t.TempDir()
	keyPathB, _ := writeKeyPairToTmp(t, dirB)
	port := startSSHServer(t, sshServerOpts{authorizedKey: pubA})
	c := newRealSSHClient("testuser", keyPathB)
	err := c.Authenticate(t.Context(), port, 5*time.Second)
	if err == nil {
		t.Fatalf("Authenticate with wrong key: want error, got nil")
	}
	if !strings.Contains(err.Error(), "ssh: handshake failed") {
		t.Fatalf("expected 'ssh: handshake failed' in error, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Tests — realQMPClient
// -----------------------------------------------------------------------------

// startQMPServer spins up a TCP listener that emits a canned QMP greeting,
// reads exactly one client command (qmp_capabilities), responds, then
// captures whatever the client sends next (typically system_powerdown).
func startQMPServer(t *testing.T, capturedCommand *string, mu *sync.Mutex) int {
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
		// Greeting.
		_, _ = io.WriteString(conn, `{"QMP":{"version":{"qemu":{"major":8,"minor":0}},"capabilities":[]}}`+"\n")
		reader := bufio.NewReader(conn)
		// Read qmp_capabilities request.
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		// Respond.
		_, _ = io.WriteString(conn, `{"return":{}}`+"\n")
		// Capture the next command (e.g. system_powerdown).
		next, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if mu != nil && capturedCommand != nil {
			mu.Lock()
			*capturedCommand = next
			mu.Unlock()
		}
		// Acknowledge so SystemPowerdown's caller sees a clean state if
		// it ever decides to read.
		_, _ = io.WriteString(conn, `{"return":{}}`+"\n")
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return port
}

func TestRealQMPClient_Dial_NegotiatesCapabilities(t *testing.T) {
	port := startQMPServer(t, nil, nil)
	c := &realQMPClient{}
	if err := c.Dial(t.Context(), port, 5*time.Second); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if c.conn == nil {
		t.Fatalf("conn nil after successful Dial")
	}
}

func TestRealQMPClient_SystemPowerdown_SendsExpectedJSON(t *testing.T) {
	var (
		mu     sync.Mutex
		gotCmd string
	)
	port := startQMPServer(t, &gotCmd, &mu)
	c := &realQMPClient{}
	if err := c.Dial(t.Context(), port, 5*time.Second); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.SystemPowerdown(t.Context()); err != nil {
		t.Fatalf("SystemPowerdown: %v", err)
	}
	// Give the server a moment to read the command.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := gotCmd
		mu.Unlock()
		if got != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(gotCmd, `"execute":"system_powerdown"`) {
		t.Fatalf(`SystemPowerdown payload: want substring "\"execute\":\"system_powerdown\"" got %q`, gotCmd)
	}
}

// -----------------------------------------------------------------------------
// Sanity check — defaultSSHClient honours environment overrides
// -----------------------------------------------------------------------------

func TestDefaultSSHClient_HonoursEnvVars(t *testing.T) {
	t.Setenv("LAVA_VM_SSH_USER", "alice")
	t.Setenv("LAVA_VM_SSH_KEY", "/tmp/some-key")
	c, ok := defaultSSHClient().(*realSSHClient)
	if !ok {
		t.Fatalf("defaultSSHClient returned %T, want *realSSHClient", defaultSSHClient())
	}
	if c.user != "alice" {
		t.Fatalf("user: want alice got %q", c.user)
	}
	if c.keyPath != "/tmp/some-key" {
		t.Fatalf("keyPath: want /tmp/some-key got %q", c.keyPath)
	}
}
