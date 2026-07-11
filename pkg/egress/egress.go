// Package egress provides VPN-host egress routing + diagnosis for reaching
// upstreams that are network-level blocked from the local/datacenter host.
//
// # The problem this solves
//
// Datacenter / cloud host IPs are frequently network-blocked for certain
// upstreams (e.g. the Russian torrent trackers): DNS resolution fails, TLS is
// MITM'd with a self-signed cert, or the connection is refused outright. This
// is ISP/DPI blocking, NOT a Cloudflare challenge — so a challenge-solver
// (FlareSolverr) cannot help. The fix is to egress through a host whose network
// CAN reach the upstream (a VPN-connected host).
//
// # The mechanism
//
//   - TunnelUp establishes a dynamic SOCKS5 forward to a VPN-connected host:
//     `ssh -D 127.0.0.1:<port> -N <vpnhost>`. All traffic sent through the
//     local SOCKS proxy exits via the VPN host's network.
//   - Verify proves the routing works: it fetches an IP-echo URL THROUGH the
//     SOCKS proxy and returns the egress IP the upstream sees, plus a per-target
//     HTTP status code. A different egress IP + a 200 from a previously-blocked
//     target confirms the route. Name resolution happens proxy-side (remote
//     DNS) — the local host's DNS is often part of the block, so this matters.
//   - DirectEgressIP fetches the same IP-echo URL WITHOUT the proxy, giving the
//     host's own egress IP for the before/after diagnosis.
//
// This package is the generic capability (consumed by Boba-Base + Lava + future
// projects); thin per-repo glue points services at the proxy address.
package egress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"digital.vasic.containers/pkg/logging"
)

// execCommand is the process-spawn seam for the ssh child. Production uses
// exec.Command; tests substitute a fake child to exercise the ssh-child
// lifecycle (fast-fail, dead-child, foreign-listener) WITHOUT a real ssh/VPN
// (§11.4.27 — no real ssh in tests). Mirrors pkg/vm's killByPortHook idiom.
var execCommand = exec.Command

// childReadyGrace is how long TunnelUp keeps watching an ssh child AFTER the
// SOCKS port first dials, before trusting the tunnel. A fast-failing ssh child
// behind a FOREIGN pre-bound listener otherwise masquerades as ready: the port
// dials (someone else's listener) while our child has already died. The grace
// window lets the child's exit win that race deterministically. It applies once,
// only on the iteration the port first becomes dialable, so it adds negligible
// latency to a genuine multi-second ssh setup.
const childReadyGrace = 150 * time.Millisecond

// Options configures a dynamic SOCKS5 egress tunnel to a VPN-connected host.
type Options struct {
	// VPNHost is the ssh destination (an ssh_config alias, "host", or
	// "user@host") of the VPN-connected host that egress is routed through.
	VPNHost string

	// User, when set, is prepended as "User@VPNHost" (ignored if VPNHost
	// already contains "@").
	User string

	// LocalAddr is the loopback bind address for the SOCKS proxy.
	// Defaults to 127.0.0.1 (localhost-only — do not expose the proxy).
	LocalAddr string

	// LocalPort is the local SOCKS5 port. Required.
	LocalPort int

	// SSHPort is the VPN host's SSH port (default 22).
	SSHPort int

	// KeyPath is an optional ssh identity file.
	KeyPath string

	// ConnectTimeout bounds the SSH connection establishment (default 10s).
	ConnectTimeout time.Duration

	// ExtraArgs are appended verbatim before the destination (advanced use).
	ExtraArgs []string
}

func (o Options) localAddr() string {
	if o.LocalAddr != "" {
		return o.LocalAddr
	}
	return "127.0.0.1"
}

func (o Options) sshPort() int {
	if o.SSHPort > 0 {
		return o.SSHPort
	}
	return 22
}

func (o Options) destination() string {
	if strings.Contains(o.VPNHost, "@") || o.User == "" {
		return o.VPNHost
	}
	return o.User + "@" + o.VPNHost
}

// SocksAddr is the "addr:port" the tunnel's SOCKS5 proxy listens on.
func (o Options) SocksAddr() string {
	return net.JoinHostPort(o.localAddr(), strconv.Itoa(o.LocalPort))
}

// buildDynamicForwardArgs renders the EXACT ssh argument vector for the dynamic
// SOCKS forward. Kept pure so it can be asserted deterministically: it MUST set
// up a `-D` dynamic forward and `-N` (no remote command), and use
// ExitOnForwardFailure so a failed bind aborts rather than silently giving a
// dead proxy.
func buildDynamicForwardArgs(o Options) []string {
	connectTimeout := o.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	args := []string{
		"-N",
		"-D", o.SocksAddr(),
		"-o", "StrictHostKeyChecking=no",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(connectTimeout.Seconds())),
		"-p", strconv.Itoa(o.sshPort()),
	}
	if o.KeyPath != "" {
		args = append(args, "-i", o.KeyPath)
	}
	args = append(args, o.ExtraArgs...)
	args = append(args, o.destination())
	return args
}

// Tunnel is a running SOCKS5 egress tunnel.
type Tunnel struct {
	socksAddr string
	logger    logging.Logger

	// stderr captures the ssh child's stderr so its real failure cause (auth,
	// bind, host-down) is surfaced instead of a generic readiness timeout.
	stderr *bytes.Buffer
	// waitDone is closed once the single cmd.Wait() has reaped the ssh child;
	// waitErr then holds that Wait result. Both are read only after a receive
	// from waitDone (a happens-before edge), so they need no extra locking.
	waitDone chan struct{}
	waitErr  error

	// mu guards cmd + down so Down is concurrency-safe + clean-idempotent.
	mu   sync.Mutex
	cmd  *exec.Cmd
	down bool
}

// Addr returns the SOCKS5 proxy "addr:port".
func (t *Tunnel) Addr() string { return t.socksAddr }

// TunnelUp establishes the SOCKS5 tunnel and blocks until the local proxy port
// accepts connections (or the timeout elapses). On failure it tears down the
// ssh process and returns an error.
func TunnelUp(ctx context.Context, o Options, logger logging.Logger) (*Tunnel, error) {
	if logger == nil {
		logger = logging.NopLogger{}
	}
	if o.VPNHost == "" {
		return nil, fmt.Errorf("egress: VPNHost is required")
	}
	if o.LocalPort <= 0 {
		return nil, fmt.Errorf("egress: LocalPort is required")
	}

	args := buildDynamicForwardArgs(o)
	cmd := execCommand("ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	logger.Info("egress: starting SOCKS tunnel via %s on %s", o.destination(), o.SocksAddr())
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("egress: start ssh: %w", err)
	}

	t := &Tunnel{
		socksAddr: o.SocksAddr(),
		logger:    logger,
		stderr:    &stderr,
		waitDone:  make(chan struct{}),
		cmd:       cmd,
	}
	// Watch the child: the SINGLE cmd.Wait() reaps it and records the exit cause.
	// The moment it dies, waitDone is closed so the readiness loop can break out
	// with the real ssh error instead of dialing a port the child never bound.
	go func() {
		t.waitErr = cmd.Wait()
		close(t.waitDone)
	}()

	readyTimeout := o.ConnectTimeout
	if readyTimeout <= 0 {
		readyTimeout = 12 * time.Second
	}
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			_ = t.Down()
			return nil, ctx.Err()
		}
		// Child died before the proxy came up (VPN host down / auth / bind
		// failure) — surface the real cause, never the generic timeout.
		select {
		case <-t.waitDone:
			err := t.childError()
			_ = t.Down()
			return nil, err
		default:
		}
		conn, err := net.DialTimeout("tcp", t.socksAddr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			// Port is dialable — but confirm OUR ssh child is alive AND stays
			// alive through the grace window. A dead child behind a foreign
			// pre-bound listener otherwise reports ready (false positive).
			select {
			case <-t.waitDone:
				err := t.childError()
				_ = t.Down()
				return nil, err
			case <-time.After(childReadyGrace):
			}
			select {
			case <-t.waitDone:
				err := t.childError()
				_ = t.Down()
				return nil, err
			default:
			}
			logger.Info("egress: SOCKS proxy ready on %s", t.socksAddr)
			return t, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = t.Down()
	return nil, fmt.Errorf("egress: SOCKS proxy %s did not become ready within %s", t.socksAddr, readyTimeout)
}

// childError builds the tunnel-failure error from the reaped ssh child's exit
// status and captured stderr. It MUST be called only after a receive from
// t.waitDone (which makes t.waitErr + t.stderr safe to read).
func (t *Tunnel) childError() error {
	stderr := strings.TrimSpace(t.stderr.String())
	base := fmt.Sprintf("egress: ssh tunnel to %s exited before the SOCKS proxy became ready", t.socksAddr)
	switch {
	case t.waitErr != nil && stderr != "":
		return fmt.Errorf("%s: %v: %s", base, t.waitErr, stderr)
	case t.waitErr != nil:
		return fmt.Errorf("%s: %v", base, t.waitErr)
	case stderr != "":
		return fmt.Errorf("%s: %s", base, stderr)
	default:
		return fmt.Errorf("%s", base)
	}
}

// Down stops the tunnel. Concurrency-safe + clean-idempotent: a sync.Mutex + a
// down flag ensure the ssh child is killed + reaped exactly once (the single
// cmd.Wait() runs in the watcher goroutine), and t.cmd is niled post-teardown.
func (t *Tunnel) Down() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.down {
		t.mu.Unlock()
		return nil
	}
	t.down = true
	cmd := t.cmd
	waitDone := t.waitDone
	t.cmd = nil
	t.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	if waitDone != nil {
		<-waitDone // reap via the single cmd.Wait() in the watcher goroutine
	} else {
		_, _ = cmd.Process.Wait()
	}
	return nil
}

// VerifyResult is the outcome of an egress check.
type VerifyResult struct {
	// EgressIP is the public IP the upstream observed for traffic sent through
	// the proxy (read from the IP-echo URL's response body).
	EgressIP string
	// Targets maps each probed target URL to its HTTP status code (0 = the
	// request failed entirely, e.g. DNS/TLS/refused).
	Targets map[string]int
}

// Verify routes through the SOCKS5 proxy at socksAddr: it reads the egress IP
// from ipEchoURL and records the HTTP status of each target. It returns an
// error if the IP-echo request itself fails (e.g. the proxy is dead), so a
// dead proxy is never reported as a successful verification.
func Verify(ctx context.Context, socksAddr, ipEchoURL string, targets []string) (*VerifyResult, error) {
	client, err := socksClient(socksAddr, 15*time.Second)
	if err != nil {
		return nil, err
	}
	ip, err := fetchEgressIP(ctx, client, ipEchoURL)
	if err != nil {
		return nil, fmt.Errorf("egress: verify via %s: %w", socksAddr, err)
	}
	res := &VerifyResult{EgressIP: ip, Targets: map[string]int{}}
	for _, tgt := range targets {
		res.Targets[tgt] = probeStatus(ctx, client, tgt)
	}
	return res, nil
}

// DirectEgressIP reads the host's own egress IP from ipEchoURL WITHOUT any
// proxy — the "direct" half of the before/after diagnosis.
func DirectEgressIP(ctx context.Context, ipEchoURL string) (string, error) {
	// DisableKeepAlives mirrors socksClient: this one-shot transport is never
	// reused, so a kept-alive idle connection + its readLoop goroutine would leak.
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}
	ip, err := fetchEgressIP(ctx, client, ipEchoURL)
	if err != nil {
		return "", fmt.Errorf("egress: direct egress IP: %w", err)
	}
	return ip, nil
}

// socksClient builds an http.Client whose transport routes through the given
// SOCKS5 proxy. Go's net/http dials socks5:// natively (no x/net/proxy dep) and
// performs proxy-side (remote) DNS for it.
func socksClient(socksAddr string, timeout time.Duration) (*http.Client, error) {
	proxyURL, err := url.Parse("socks5://" + socksAddr)
	if err != nil {
		return nil, fmt.Errorf("egress: bad socks addr %q: %w", socksAddr, err)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}, nil
}

func fetchEgressIP(ctx context.Context, client *http.Client, ipEchoURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipEchoURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	// A blocked/MITM'd IP-echo — the EXACT scenario this package diagnoses —
	// answers with a non-2xx status (403 "Access Denied", 500, a 302+body).
	// Using such a response would report a bogus egress IP with err==nil, so a
	// dead/blocked echo would masquerade as a successful diagnosis. Require 2xx.
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("egress: IP-echo returned HTTP %d (body %q)", resp.StatusCode, ip)
	}
	if ip == "" {
		return "", fmt.Errorf("empty IP-echo response (status %d)", resp.StatusCode)
	}
	// The body MUST be a bare IP address (the documented IP-echo contract). A 2xx
	// with an HTML/error body (a soft block) is rejected so "Access Denied" is
	// never returned as though it were the egress IP.
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("egress: IP-echo body is not an IP address: %q", ip)
	}
	return ip, nil
}

func probeStatus(ctx context.Context, client *http.Client, target string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode
}
