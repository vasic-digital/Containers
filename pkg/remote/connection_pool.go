package remote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
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
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf(
			"create control socket dir %s: %w", dir, err,
		)
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

	socketPath := filepath.Join(
		p.socketDir,
		fmt.Sprintf("ctrl-%s-%d", host.Address, host.SSHPort()),
	)

	args := p.masterArgs(host, socketPath)
	execCtx, cancel := context.WithTimeout(
		ctx, p.opts.ConnectTimeout,
	)
	defer cancel()

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
func (p *ConnectionPool) Release(host RemoteHost) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := hostKey(host)
	if entry, ok := p.active[key]; ok {
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

	if host.KeyPath != "" {
		args = append(args, "-i", host.KeyPath)
	}

	args = append(args,
		fmt.Sprintf("%s@%s", host.User, host.Address),
	)
	return args
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
