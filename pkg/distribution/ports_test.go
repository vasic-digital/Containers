package distribution

import (
	"testing"

	"digital.vasic.containers/pkg/scheduler"
)

// TestBuildPublishFlags verifies the remote `run` port-publish fragment.
// Without this, a distributed container's service is reachable only inside
// its network namespace and a cross-host health check cannot reach it.
func TestBuildPublishFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []scheduler.PortMapping
		want string
	}{
		{"nil", nil, ""},
		{"empty", []scheduler.PortMapping{}, ""},
		{"host:container default tcp", []scheduler.PortMapping{{HostPort: 8080, ContainerPort: 80}}, " -p 8080:80/tcp"},
		{"explicit udp, ephemeral host", []scheduler.PortMapping{{ContainerPort: 53, Protocol: "udp"}}, " -p 53/udp"},
		{"multiple", []scheduler.PortMapping{{HostPort: 1, ContainerPort: 2}, {HostPort: 3, ContainerPort: 4, Protocol: "tcp"}}, " -p 1:2/tcp -p 3:4/tcp"},
		{"skip non-positive container port", []scheduler.PortMapping{{HostPort: 9, ContainerPort: 0}}, ""},
		{"uppercase protocol normalized", []scheduler.PortMapping{{HostPort: 5, ContainerPort: 6, Protocol: "TCP"}}, " -p 5:6/tcp"},
		{"sctp allowed", []scheduler.PortMapping{{HostPort: 7, ContainerPort: 8, Protocol: "sctp"}}, " -p 7:8/sctp"},
		{"injection attempt skipped (allowlist)", []scheduler.PortMapping{{HostPort: 9, ContainerPort: 10, Protocol: "tcp; rm -rf /"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildPublishFlags(c.in); got != c.want {
				t.Errorf("buildPublishFlags(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
