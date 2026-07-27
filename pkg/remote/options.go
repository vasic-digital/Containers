package remote

import "time"

// Options holds configuration for SSH-based remote execution.
type Options struct {
	// ConnectTimeout is the SSH connection timeout.
	ConnectTimeout time.Duration
	// CommandTimeout is the default timeout for remote commands.
	CommandTimeout time.Duration
	// MaxConnections is the maximum number of concurrent SSH
	// connections per host.
	MaxConnections int
	// KeepAlive is the interval for SSH keep-alive messages
	// (maps to ssh -o ServerAliveInterval). Set to 0 to disable.
	KeepAlive time.Duration
	// KeepAliveCountMax is the maximum number of missed keep-alive
	// probes before SSH terminates the session (maps to ssh -o
	// ServerAliveCountMax). Combined with KeepAlive this is the
	// total tolerated network silence: KeepAlive * KeepAliveCountMax.
	KeepAliveCountMax int
	// StrictHostKeyCheck enables SSH strict host key checking.
	StrictHostKeyCheck bool
	// ControlMasterEnabled enables SSH ControlMaster multiplexing.
	ControlMasterEnabled bool
	// ControlSocketDir is the directory for ControlMaster sockets.
	ControlSocketDir string
	// ControlPersist is the duration to keep ControlMaster alive
	// after the last connection closes.
	ControlPersist time.Duration
}

// DefaultOptions returns Options with sensible defaults.
func DefaultOptions() Options {
	return Options{
		ConnectTimeout: 10 * time.Second,
		// 30 minutes tolerates multi-GB image pulls and multi-layer
		// builds during `compose up`. SSH keep-alive (KeepAlive *
		// KeepAliveCountMax) detects genuinely dead connections
		// much sooner; this cap is the backstop for remote commands
		// that simply take a long time to complete. The previous
		// 300s / 5-minute default was too aggressive for cold-cache
		// hosts building Dockerfiles from scratch.
		CommandTimeout:       1800 * time.Second,
		MaxConnections:       5,
		KeepAlive:            30 * time.Second,
		KeepAliveCountMax:    10,
		StrictHostKeyCheck:   false,
		ControlMasterEnabled: true,
		ControlSocketDir:     "/tmp/containers-ssh-ctrl",
		ControlPersist:       5 * time.Minute,
	}
}

// Option configures remote execution behavior.
type Option func(*Options)

// ApplyOptions creates Options from the defaults plus all given
// Option functions.
func ApplyOptions(opts []Option) Options {
	o := DefaultOptions()
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithConnectTimeout sets the SSH connection timeout.
func WithConnectTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.ConnectTimeout = d
	}
}

// WithCommandTimeout sets the default command execution timeout.
func WithCommandTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.CommandTimeout = d
	}
}

// WithMaxConnections sets the maximum concurrent SSH connections
// per host.
func WithMaxConnections(n int) Option {
	return func(o *Options) {
		o.MaxConnections = n
	}
}

// WithKeepAlive sets the SSH keep-alive interval (ServerAliveInterval).
// Set to 0 to disable keep-alive probing.
func WithKeepAlive(d time.Duration) Option {
	return func(o *Options) {
		o.KeepAlive = d
	}
}

// WithKeepAliveCountMax sets the maximum number of missed SSH
// keep-alive probes tolerated before the session is terminated
// (ServerAliveCountMax). Total network silence tolerance is
// KeepAlive * KeepAliveCountMax.
func WithKeepAliveCountMax(n int) Option {
	return func(o *Options) {
		o.KeepAliveCountMax = n
	}
}

// WithStrictHostKeyCheck enables or disables strict host key
// checking.
func WithStrictHostKeyCheck(enabled bool) Option {
	return func(o *Options) {
		o.StrictHostKeyCheck = enabled
	}
}

// WithControlMaster enables or disables SSH ControlMaster
// multiplexing.
func WithControlMaster(enabled bool) Option {
	return func(o *Options) {
		o.ControlMasterEnabled = enabled
	}
}

// WithControlSocketDir sets the directory for ControlMaster sockets.
func WithControlSocketDir(dir string) Option {
	return func(o *Options) {
		o.ControlSocketDir = dir
	}
}

// WithControlPersist sets how long ControlMaster connections
// persist after the last session closes.
func WithControlPersist(d time.Duration) Option {
	return func(o *Options) {
		o.ControlPersist = d
	}
}
