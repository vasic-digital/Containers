package compose

// --- Up Options ---

type upOptions struct {
	Detach        bool
	RemoveOrphans bool
	BuildFirst    bool
	ForceRecreate bool
	NoRecreate    bool
	Timeout       int
	Wait          bool
	// WaitTimeout is the caller's wait-deadline (in seconds) for services to
	// become running|healthy. It is DISTINCT from Timeout, which maps to
	// compose's own `--timeout` graceful-shutdown flag (CO-2) -- conflating
	// the two silently capped (or extended) the wait deadline to whatever the
	// caller set for shutdown grace. On the podman host-side poll
	// (waitForServices) 0 means "use the package default" (defaultWaitTimeout,
	// 120s). On docker compose it is forwarded to the native `--wait-timeout`
	// flag when > 0 (COMP2-1); 0 leaves docker's `--wait` on its own built-in
	// default.
	WaitTimeout int
}

// UpOption configures compose up behavior.
type UpOption func(*upOptions)

func defaultUpOptions() *upOptions {
	return &upOptions{
		Detach:        true,
		RemoveOrphans: false,
		BuildFirst:    false,
		ForceRecreate: false,
		NoRecreate:    false,
		Timeout:       0,
		Wait:          false,
	}
}

func applyUpOptions(opts []UpOption) *upOptions {
	o := defaultUpOptions()
	for _, fn := range opts {
		fn(o)
	}
	return o
}

// WithUpDetach sets whether compose up runs in detached mode.
func WithUpDetach(detach bool) UpOption {
	return func(o *upOptions) {
		o.Detach = detach
	}
}

// WithRemoveOrphans removes containers for services not defined in the
// compose file.
func WithRemoveOrphans(remove bool) UpOption {
	return func(o *upOptions) {
		o.RemoveOrphans = remove
	}
}

// WithBuildFirst forces a build before starting containers.
func WithBuildFirst(build bool) UpOption {
	return func(o *upOptions) {
		o.BuildFirst = build
	}
}

// WithForceRecreate forces recreation of containers even if their
// configuration has not changed.
func WithForceRecreate(force bool) UpOption {
	return func(o *upOptions) {
		o.ForceRecreate = force
	}
}

// WithNoRecreate prevents recreation of existing containers.
func WithNoRecreate(noRecreate bool) UpOption {
	return func(o *upOptions) {
		o.NoRecreate = noRecreate
	}
}

// WithUpTimeout sets the shutdown timeout in seconds.
func WithUpTimeout(seconds int) UpOption {
	return func(o *upOptions) {
		o.Timeout = seconds
	}
}

// WithWait waits for services to be running|healthy before returning.
func WithWait(wait bool) UpOption {
	return func(o *upOptions) {
		o.Wait = wait
	}
}

// WithWaitTimeout sets the caller's wait-deadline (in seconds) for services
// to become running|healthy -- distinct from WithUpTimeout, which sets
// compose's own `--timeout` graceful-shutdown flag. On podman-compose it
// bounds the host-side wait-fallback poll (waitForServices); 0 (the default)
// uses the package default (120s). On docker compose it is forwarded to the
// native `--wait-timeout` flag when > 0 (COMP2-1); when unset (0) docker's
// `--wait` keeps its own built-in default. Only meaningful alongside
// WithWait(true).
func WithWaitTimeout(seconds int) UpOption {
	return func(o *upOptions) {
		o.WaitTimeout = seconds
	}
}

// --- Down Options ---

type downOptions struct {
	RemoveOrphans bool
	RemoveVolumes bool
	RemoveImages  string
	Timeout       int
}

// DownOption configures compose down behavior.
type DownOption func(*downOptions)

func defaultDownOptions() *downOptions {
	return &downOptions{
		RemoveOrphans: false,
		RemoveVolumes: false,
		RemoveImages:  "",
		Timeout:       0,
	}
}

func applyDownOptions(opts []DownOption) *downOptions {
	o := defaultDownOptions()
	for _, fn := range opts {
		fn(o)
	}
	return o
}

// WithDownRemoveOrphans removes containers for services not defined in
// the compose file during down.
func WithDownRemoveOrphans(remove bool) DownOption {
	return func(o *downOptions) {
		o.RemoveOrphans = remove
	}
}

// WithDownRemoveVolumes removes named volumes declared in the compose
// file and anonymous volumes attached to containers.
func WithDownRemoveVolumes(remove bool) DownOption {
	return func(o *downOptions) {
		o.RemoveVolumes = remove
	}
}

// WithDownRemoveImages removes images when down completes. Valid values
// are "all" and "local".
func WithDownRemoveImages(mode string) DownOption {
	return func(o *downOptions) {
		o.RemoveImages = mode
	}
}

// WithDownTimeout sets the shutdown timeout in seconds.
func WithDownTimeout(seconds int) DownOption {
	return func(o *downOptions) {
		o.Timeout = seconds
	}
}
