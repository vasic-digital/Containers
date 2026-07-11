package remote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	iexec "digital.vasic.containers/internal/exec"
)

// ConnectionPool manages SSH ControlMaster connections for
// multiplexing multiple sessions over a single TCP connection.
type ConnectionPool struct {
	mu         sync.Mutex
	opts       Options
	active     map[string]*controlEntry
	dialLocks  map[string]*sync.Mutex // per-host, serializes same-host dials
	socketDir  string
	cleanupCtx context.Context
	cleanupFn  context.CancelFunc
}

type controlEntry struct {
	host       RemoteHost
	socketPath string
	refs       int
	createdAt  time.Time
}

// NewConnectionPool creates a ConnectionPool that stores control
// sockets in the configured directory.
func NewConnectionPool(opts Options) (*ConnectionPool, error) {
	dir := opts.ControlSocketDir
	if dir == "" {
		dir = "/tmp/containers-ssh-ctrl"
	}

	// REMOTE-MED-4: os.MkdirAll(dir, 0700) only enforces the 0700 mode
	// when it actually CREATES dir. A pre-existing directory — left
	// over from a previous run, or pre-created by a different local
	// user — is otherwise silently trusted even if its permissions are
	// looser, and the control-socket filenames inside it are fully
	// predictable (ctrl-<address>-<port>), so a hostile co-resident
	// user could pre-stage a world-readable/writable directory to
	// intercept or tamper with ControlMaster sockets. Detect whether
	// dir already existed BEFORE MkdirAll (which is a no-op on an
	// existing directory and does not change its mode/owner), then
	// verify it is owned by the current user and not writable by
	// group/other (see verifySocketDirOwnership for why this checks
	// the write bits rather than an exact mode).
	preExisted := isExistingDir(dir)

	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf(
			"create control socket dir %s: %w", dir, err,
		)
	}

	if preExisted {
		if err := verifySocketDirOwnership(dir); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := &ConnectionPool{
		opts:       opts,
		active:     make(map[string]*controlEntry),
		socketDir:  dir,
		cleanupCtx: ctx,
		cleanupFn:  cancel,
	}
	return pool, nil
}

// isExistingDir reports whether path already exists and is a
// directory.
func isExistingDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// verifySocketDirOwnership refuses to trust a pre-existing control
// socket directory unless (a) it is owned by the current user and
// (b) it grants NO write access to group or other (REMOTE-MED-4).
// Socket filenames under this directory are fully predictable
// (ctrl-<address>-<port>), so a directory owned by a different local
// user, or one writable by group/other, would let that other user
// pre-stage, replace, or race a ControlMaster socket at a name we are
// about to use.
//
// This deliberately checks the WRITE bits rather than requiring an
// exact 0700 match: os.MkdirAll(dir, 0700) only forces 0700 when it
// creates dir itself; legitimately-owned pre-existing directories
// (e.g. a test harness's t.TempDir(), which is created via
// os.Mkdir(dir, 0777) and therefore lands at whatever the process
// umask allows — commonly 0755) are safe to reuse as long as no other
// user can write into them. Rejecting every mode other than exactly
// 0700 would also reject those same-owner, non-writable-by-others
// directories, which is not the actual security boundary here.
func verifySocketDirOwnership(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf(
			"stat pre-existing control socket dir %s: %w", dir, err,
		)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if uid := int(stat.Uid); uid != os.Getuid() {
			return fmt.Errorf(
				"control socket dir %s already existed owned by uid "+
					"%d (want %d, the current user) — refusing to "+
					"trust a pre-existing directory owned by another "+
					"user",
				dir, uid, os.Getuid(),
			)
		}
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf(
			"control socket dir %s already existed writable by "+
				"group or other (mode %04o) — refusing to trust a "+
				"pre-existing directory that another local user may "+
				"be able to write into",
			dir, perm,
		)
	}
	return nil
}

// Acquire returns the path to a ControlMaster socket for the given
// host, creating the master connection if necessary.
func (p *ConnectionPool) Acquire(
	ctx context.Context, host RemoteHost,
) (string, error) {
	key := hostKey(host)

	// Fast path under the pool lock: reuse a pooled master, but ONLY if
	// its control socket still exists. A ControlMaster that died
	// out-of-band (remote reboot, network partition outliving the SSH
	// keepalive budget, master process killed) leaves a stale entry whose
	// socket is gone; handing that dead path back would report a healthy
	// pooled connection for one that cannot be used (Gotcha #1). Evict on
	// a missing socket and fall through to a fresh dial.
	p.mu.Lock()
	if entry, ok := p.active[key]; ok {
		if _, statErr := os.Stat(entry.socketPath); statErr == nil {
			entry.refs++
			sp := entry.socketPath
			p.mu.Unlock()
			return sp, nil
		}
		delete(p.active, key)
	}
	dl := p.dialLockLocked(key)
	p.mu.Unlock()

	// Serialize dials for THIS host only (two goroutines must not race two
	// ControlMasters onto the same deterministic socket path), while
	// leaving the pool mutex free so dials for OTHER hosts — and
	// Release/Close/CloseHost/ActiveCount — run concurrently. Holding the
	// pool-wide mutex across the blocking ssh dial (up to ConnectTimeout)
	// would serialize every pool operation behind one slow/hung host.
	dl.Lock()
	defer dl.Unlock()

	// Re-check under the pool lock: a concurrent same-host Acquire may
	// have finished dialing while we waited on the per-host dial lock.
	p.mu.Lock()
	if entry, ok := p.active[key]; ok {
		if _, statErr := os.Stat(entry.socketPath); statErr == nil {
			entry.refs++
			sp := entry.socketPath
			p.mu.Unlock()
			return sp, nil
		}
		delete(p.active, key)
	}
	p.mu.Unlock()

	socketPath := p.controlSocketPath(host)

	args := p.masterArgs(host, socketPath)
	// Only impose a deadline when ConnectTimeout is positive. A zero
	// ConnectTimeout means "no artificial deadline" (matching the
	// KeepAlive/CommandTimeout 0=disable convention and ssh's own
	// `-o ConnectTimeout=0` = no-limit); passing 0 to context.WithTimeout
	// would produce an ALREADY-EXPIRED context that cancels every dial
	// immediately, making the pool permanently unusable when a caller
	// explicitly requests ConnectTimeout=0. Mirrors the CommandTimeout
	// guard in SSHExecutor.Execute.
	execCtx := ctx
	if p.opts.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, p.opts.ConnectTimeout)
		defer cancel()
	}

	// Blocking dial runs WITHOUT the pool mutex held.
	_, stderr, err := iexec.Run(execCtx, "ssh", args...)
	if err != nil {
		return "", fmt.Errorf(
			"start ControlMaster for %s: %w (stderr: %s)",
			key, err, stderr,
		)
	}

	p.mu.Lock()
	p.active[key] = &controlEntry{
		host:       host,
		socketPath: socketPath,
		refs:       1,
		createdAt:  time.Now(),
	}
	p.mu.Unlock()
	return socketPath, nil
}

// dialLockLocked returns the per-host dial mutex, creating it on first
// use. The caller MUST hold p.mu.
func (p *ConnectionPool) dialLockLocked(key string) *sync.Mutex {
	if p.dialLocks == nil {
		p.dialLocks = make(map[string]*sync.Mutex)
	}
	dl, ok := p.dialLocks[key]
	if !ok {
		dl = &sync.Mutex{}
		p.dialLocks[key] = dl
	}
	return dl
}

// Release decrements the reference count for a host's connection.
// When refs reaches zero the entry is kept alive for reuse until
// ControlPersist expires or Close is called.
//
// REMOTE-MED-3: Release is keyed only by host, with no per-caller
// token to verify THIS caller actually holds the ref it is about to
// drop. Execute unconditionally `defer`s Release even though sshArgs
// silently swallows an Acquire failure (falls back to a direct
// connection without incrementing refs). If a caller whose own
// Acquire failed races with a different, live caller that
// successfully acquired the same host, the failing caller's deferred
// Release must not steal/underflow the live holder's ref count.
// Floor the decrement at zero so refs can never go negative — the
// minimal, safe hardening of the invariant regardless of which
// caller's Release fires in which order.
func (p *ConnectionPool) Release(host RemoteHost) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := hostKey(host)
	if entry, ok := p.active[key]; ok && entry.refs > 0 {
		entry.refs--
	}
}

// Close terminates all active ControlMaster connections and cleans
// up sockets.
func (p *ConnectionPool) Close() error {
	p.cleanupFn()

	p.mu.Lock()
	defer p.mu.Unlock()

	var firstErr error
	for key, entry := range p.active {
		if err := p.closeEntry(entry); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(p.active, key)
	}
	return firstErr
}

// CloseHost terminates the ControlMaster connection for a specific
// host.
func (p *ConnectionPool) CloseHost(host RemoteHost) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := hostKey(host)
	entry, ok := p.active[key]
	if !ok {
		return nil
	}
	err := p.closeEntry(entry)
	delete(p.active, key)
	return err
}

// ActiveCount returns the number of active control connections.
func (p *ConnectionPool) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

func (p *ConnectionPool) closeEntry(entry *controlEntry) error {
	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	args := []string{
		"-S", entry.socketPath,
		"-O", "exit",
		fmt.Sprintf(
			"%s@%s", entry.host.User, entry.host.Address,
		),
	}
	_, _, err := iexec.Run(ctx, "ssh", args...)
	_ = os.Remove(entry.socketPath)
	return err
}

func (p *ConnectionPool) masterArgs(
	host RemoteHost, socketPath string,
) []string {
	args := []string{
		"-fNM",
		"-S", socketPath,
		"-o", "StrictHostKeyChecking=" +
			boolToYesNo(p.opts.StrictHostKeyCheck),
		"-o", fmt.Sprintf(
			"ConnectTimeout=%d",
			int(p.opts.ConnectTimeout.Seconds()),
		),
		"-o", fmt.Sprintf(
			"ServerAliveInterval=%d",
			int(p.opts.KeepAlive.Seconds()),
		),
		"-o", "ServerAliveCountMax=3",
		"-p", strconv.Itoa(host.SSHPort()),
	}

	// REMOTE-HIGH-3: without -o ControlPersist=<seconds>, the -fNM -N
	// master started here persists until an explicit `-O exit` — it never
	// self-expires, contradicting Gotcha #1 ("the socket can outlive the
	// last Release() by ControlPersist"). Combined with any ref-count
	// leak (e.g. REMOTE-HIGH-2), that produces permanent orphan ssh
	// processes (§11.4.14 no-leak). Only impose the flag when
	// ControlPersist is positive, mirroring the ConnectTimeout==0 "no
	// artificial deadline" guard idiom used in Acquire.
	if p.opts.ControlPersist > 0 {
		args = append(args, "-o", fmt.Sprintf(
			"ControlPersist=%d",
			int(p.opts.ControlPersist.Seconds()),
		))
	}

	if host.KeyPath != "" {
		args = append(args, "-i", host.KeyPath)
	}

	args = append(args,
		fmt.Sprintf("%s@%s", host.User, host.Address),
	)
	return args
}

// controlSocketPath returns the on-disk ControlMaster socket path for
// host. RM2-2: the pool KEY (hostKey) is user@address:port, but the
// socket path historically included ONLY address+port — so two configured
// hosts that share an address:port but differ in SSH user (e.g.
// deploy@gpu1:22 and root@gpu1:22) mapped to distinct pool entries yet the
// SAME socket file. The second host's `-fNM` master dial then collided
// with the first's live socket (OpenSSH "ControlSocket already exists,
// disabling multiplexing"), so it could never pool and thrashed on every
// Acquire. The user is part of the identity, so it MUST be part of the
// socket name — matching CLAUDE.md's documented "one socket per
// (user@host:port)" contract.
func (p *ConnectionPool) controlSocketPath(host RemoteHost) string {
	return filepath.Join(
		p.socketDir,
		fmt.Sprintf("ctrl-%s-%s-%d", host.User, host.Address, host.SSHPort()),
	)
}

func hostKey(host RemoteHost) string {
	return fmt.Sprintf("%s@%s:%d",
		host.User, host.Address, host.SSHPort(),
	)
}

func boolToYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
