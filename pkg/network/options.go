package network

import "time"

// Options configures network tunnel behavior.
type Options struct {
	// PortRangeStart is the start of the local port range.
	PortRangeStart int
	// PortRangeEnd is the end of the local port range.
	PortRangeEnd int

	// AutoReconnect, ReconnectInterval, and MaxReconnectAttempts are NOT YET
	// IMPLEMENTED. They are config-only stubs — DefaultOptions() defaults
	// AutoReconnect to true and the With* setters store all three, but NOTHING
	// consumes them: the tunnel manager never re-establishes a dropped tunnel.
	// A tunnel whose ssh process dies is reaped (removed, or retained as
	// State=TunnelFailed on a forward-failure death) — it is never reconnected,
	// and the State=TunnelReconnecting value is likewise never set. These fields
	// are reserved as the seam for a future reconnection loop (a feature pending
	// an operator decision); until it lands, setting AutoReconnect=true has no
	// runtime effect. Callers MUST NOT rely on automatic reconnection. Mirrors
	// the honest stub disclaimer on overlay.go's TunnelOverlay.

	// AutoReconnect is reserved to enable automatic tunnel reconnection.
	// NOT YET IMPLEMENTED — currently a no-op (see the note above).
	AutoReconnect bool
	// ReconnectInterval is reserved as the delay between reconnect attempts.
	// NOT YET IMPLEMENTED — currently unused (see the note above).
	ReconnectInterval time.Duration
	// MaxReconnectAttempts is reserved to limit reconnect retries (0=unlimited).
	// NOT YET IMPLEMENTED — currently unused (see the note above).
	MaxReconnectAttempts int
}

// DefaultOptions returns sensible defaults.
func DefaultOptions() Options {
	return Options{
		PortRangeStart:       20000,
		PortRangeEnd:         30000,
		AutoReconnect:        true,
		ReconnectInterval:    5 * time.Second,
		MaxReconnectAttempts: 10,
	}
}

// Option configures network behavior.
type Option func(*Options)

// ApplyOptions builds Options from defaults and given options.
func ApplyOptions(opts []Option) Options {
	o := DefaultOptions()
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithPortRange sets the local port allocation range.
func WithPortRange(start, end int) Option {
	return func(o *Options) {
		o.PortRangeStart = start
		o.PortRangeEnd = end
	}
}

// WithAutoReconnect enables or disables automatic reconnection.
// NOT YET IMPLEMENTED — stores the flag but has no runtime effect; see the
// Options.AutoReconnect field note.
func WithAutoReconnect(enabled bool) Option {
	return func(o *Options) { o.AutoReconnect = enabled }
}

// WithReconnectInterval sets the delay between reconnect
// attempts. NOT YET IMPLEMENTED — stores the value but has no runtime effect;
// see the Options.ReconnectInterval field note.
func WithReconnectInterval(d time.Duration) Option {
	return func(o *Options) { o.ReconnectInterval = d }
}

// WithMaxReconnectAttempts sets the maximum reconnect retries.
// NOT YET IMPLEMENTED — stores the value but has no runtime effect; see the
// Options.MaxReconnectAttempts field note.
func WithMaxReconnectAttempts(n int) Option {
	return func(o *Options) { o.MaxReconnectAttempts = n }
}
