package endpoint

import (
	"strings"
	"time"

	"digital.vasic.containers/internal/netaddr"
)

// ServiceEndpoint holds the configuration for a single service
// endpoint, including connectivity, health checking, and discovery
// settings.
type ServiceEndpoint struct {
	// Host is the hostname or IP address of the service.
	Host string
	// Port is the port number the service listens on.
	Port string
	// URL is an explicit URL override. When set, it takes
	// precedence over Host and Port for URL resolution.
	URL string

	// Enabled indicates whether this endpoint is active.
	Enabled bool
	// Required indicates whether the service must be reachable
	// for the system to start successfully.
	Required bool
	// Remote indicates the service runs on a remote host and
	// should not be managed by the local container runtime.
	Remote bool

	// HealthPath is the HTTP path used for health checks
	// (e.g., "/healthz").
	HealthPath string
	// HealthType is the type of health check to perform
	// (e.g., "http", "tcp", "grpc").
	HealthType string

	// Timeout is the maximum duration to wait for a response
	// from this endpoint.
	Timeout time.Duration
	// RetryCount is the number of retry attempts for failed
	// health checks.
	RetryCount int

	// ComposeFile is the path to the Docker Compose file that
	// defines this service.
	ComposeFile string
	// ServiceName is the name of the service within the compose
	// file.
	ServiceName string
	// Profile is the compose profile this service belongs to.
	Profile string

	// DiscoveryEnabled indicates whether service discovery is
	// active for this endpoint.
	DiscoveryEnabled bool
	// DiscoveryMethod is the discovery mechanism (e.g., "dns",
	// "consul", "static").
	DiscoveryMethod string
	// DiscoveryTimeout is the maximum duration for a discovery
	// lookup.
	DiscoveryTimeout time.Duration

	// Discovered indicates that this endpoint was resolved via
	// service discovery rather than static configuration.
	Discovered bool
}

// ResolvedURL returns the base URL for this endpoint. If URL is
// set explicitly it is returned directly. Otherwise the URL is
// constructed from Host and Port without the HealthPath.
func (e *ServiceEndpoint) ResolvedURL() string {
	if e.URL != "" {
		return e.URL
	}
	return resolveURL(e.Host, e.Port, "")
}

// resolveURL builds a URL from host, port, and an optional path.
func resolveURL(host, port, path string) string {
	if host == "" {
		host = "localhost"
	}
	// Bracket in both branches: an IPv6 literal is just as malformed inside
	// "http://::1" as it is inside "http://::1:9000". A host that is already
	// a full URL is left untouched, so the scheme check below still sees it.
	base := netaddr.BracketHost(host)
	if port != "" {
		base = netaddr.JoinHostPort(host, port)
	}
	if !strings.HasPrefix(base, "http://") &&
		!strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	if path != "" {
		path = "/" + strings.TrimLeft(path, "/")
		return base + path
	}
	return base
}
