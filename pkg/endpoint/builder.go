package endpoint

import "time"

// Builder provides a fluent API for constructing ServiceEndpoint
// instances.
type Builder struct {
	ep ServiceEndpoint
}

// NewEndpoint returns a new Builder with sensible defaults.
func NewEndpoint() *Builder {
	return &Builder{
		ep: ServiceEndpoint{
			Enabled:    true,
			HealthType: defaultHealthType,
			Timeout:    defaultTimeout,
			RetryCount: defaultRetryCount,
		},
	}
}

// WithHost sets the endpoint host.
func (b *Builder) WithHost(h string) *Builder {
	b.ep.Host = h
	return b
}

// WithPort sets the endpoint port.
func (b *Builder) WithPort(p string) *Builder {
	b.ep.Port = p
	return b
}

// WithURL sets an explicit URL override.
func (b *Builder) WithURL(u string) *Builder {
	b.ep.URL = u
	return b
}

// WithEnabled sets whether the endpoint is enabled.
func (b *Builder) WithEnabled(e bool) *Builder {
	b.ep.Enabled = e
	return b
}

// WithRequired sets whether the endpoint is required for boot.
func (b *Builder) WithRequired(r bool) *Builder {
	b.ep.Required = r
	return b
}

// WithRemote marks the endpoint as a remote service.
func (b *Builder) WithRemote(r bool) *Builder {
	b.ep.Remote = r
	return b
}

// WithHealthPath sets the health check path.
func (b *Builder) WithHealthPath(p string) *Builder {
	b.ep.HealthPath = p
	return b
}

// WithHealthType sets the health check type.
func (b *Builder) WithHealthType(t string) *Builder {
	b.ep.HealthType = t
	return b
}

// WithTimeout sets the endpoint timeout.
func (b *Builder) WithTimeout(d time.Duration) *Builder {
	b.ep.Timeout = d
	return b
}

// WithRetryCount sets the number of health check retries.
func (b *Builder) WithRetryCount(n int) *Builder {
	b.ep.RetryCount = n
	return b
}

// WithComposeFile sets the Docker Compose file path.
func (b *Builder) WithComposeFile(f string) *Builder {
	b.ep.ComposeFile = f
	return b
}

// WithServiceName sets the compose service name.
func (b *Builder) WithServiceName(n string) *Builder {
	b.ep.ServiceName = n
	return b
}

// WithProfile sets the compose profile.
func (b *Builder) WithProfile(p string) *Builder {
	b.ep.Profile = p
	return b
}

// WithDiscoveryEnabled enables or disables service discovery.
func (b *Builder) WithDiscoveryEnabled(e bool) *Builder {
	b.ep.DiscoveryEnabled = e
	return b
}

// WithDiscoveryMethod sets the discovery mechanism.
func (b *Builder) WithDiscoveryMethod(m string) *Builder {
	b.ep.DiscoveryMethod = m
	return b
}

// WithDiscoveryTimeout sets the discovery lookup timeout.
func (b *Builder) WithDiscoveryTimeout(d time.Duration) *Builder {
	b.ep.DiscoveryTimeout = d
	return b
}

// Build returns the constructed ServiceEndpoint.
//
// Build is intentionally non-validating and its signature is FROZEN for
// backward compatibility (existing `ep := b.Build()` call sites, e.g. in
// cmd/ota-device-emu-boot, must keep compiling). Use BuildValidated when
// an error path is wanted.
func (b *Builder) Build() ServiceEndpoint {
	return b.ep
}

// BuildValidated is the additive, non-breaking companion to Build: it
// returns the constructed ServiceEndpoint together with the result of
// Validate(). Callers that want an error path use this; Build() callers
// are unaffected (EP-3, §11.4.122 — no existing signature changed).
func (b *Builder) BuildValidated() (ServiceEndpoint, error) {
	ep := b.ep
	return ep, ep.Validate()
}
