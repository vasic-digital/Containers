package compose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinProbeAll makes every candidate's version probe succeed -- models a host
// where BOTH docker and podman compose are installed and working (the R1
// genuine-docker + genuine-podman scenario).
func pinProbeAll(string, []string) bool { return true }

// pinProbeOnly succeeds only for the named command; everything else fails.
func pinProbeOnly(want string) func(string, []string) bool {
	return func(cmd string, _ []string) bool { return cmd == want }
}

// pinProbeNone fails every candidate -- models a pin whose command is absent.
func pinProbeNone(string, []string) bool { return false }

// R1 core assertion: with the CONTAINERS_COMPOSE_CMD pin set to
// "podman compose", podman is selected even though docker compose also works
// and would win the docker-first auto-detect. This is the §11.4.161
// rootless-podman-only guarantee for a genuine-docker + genuine-podman host.
func TestDetectComposeCmd_PinForcesPodmanOverDocker(t *testing.T) {
	cmd, args, err := detectComposeCmdWithProbe("podman compose", pinProbeAll)
	require.NoError(t, err)
	assert.Equal(t, "podman", cmd)
	assert.Equal(t, []string{"compose"}, args)
}

// The pin also honours the standalone podman-compose spelling.
func TestDetectComposeCmd_PinHonoursPodmanCompose(t *testing.T) {
	cmd, args, err := detectComposeCmdWithProbe("podman-compose", pinProbeAll)
	require.NoError(t, err)
	assert.Equal(t, "podman-compose", cmd)
	assert.Empty(t, args)
}

// A pin that does not verify is a HARD error naming the env var -- never a
// silent fall-through to the docker-first auto-detect (the §11.4.161 footgun).
func TestDetectComposeCmd_UnrunnablePinFailsClosed(t *testing.T) {
	cmd, args, err := detectComposeCmdWithProbe("podman compose", pinProbeNone)
	require.Error(t, err)
	assert.Contains(t, err.Error(), composeEnvVar)
	assert.Contains(t, err.Error(), "not runnable")
	assert.Empty(t, cmd)
	assert.Nil(t, args)
}

// With no pin, generic auto-detect is preserved: docker-first when both are
// present ...
func TestDetectComposeCmd_NoPinAutoDetectDockerFirst(t *testing.T) {
	cmd, args, err := detectComposeCmdWithProbe("", pinProbeAll)
	require.NoError(t, err)
	assert.Equal(t, "docker", cmd)
	assert.Equal(t, []string{"compose"}, args)
}

// ... and falls through to podman when docker is absent.
func TestDetectComposeCmd_NoPinFallsThroughToPodman(t *testing.T) {
	cmd, args, err := detectComposeCmdWithProbe("", pinProbeOnly("podman"))
	require.NoError(t, err)
	assert.Equal(t, "podman", cmd)
	assert.Equal(t, []string{"compose"}, args)
}

// End-to-end via the public detectComposeCmd() + real env wiring, with the
// package probe var swapped for a fake (still hermetic) -- proves
// os.Getenv(CONTAINERS_COMPOSE_CMD) is actually read and wired to the pin.
func TestDetectComposeCmd_EnvPinWiredThroughPublicEntry(t *testing.T) {
	orig := composeCmdProbe
	t.Cleanup(func() { composeCmdProbe = orig })
	composeCmdProbe = pinProbeAll // docker would win auto-detect

	t.Setenv(composeEnvVar, "podman compose")

	cmd, args, err := detectComposeCmd()
	require.NoError(t, err)
	assert.Equal(t, "podman", cmd)
	assert.Equal(t, []string{"compose"}, args)
}
