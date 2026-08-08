package lifecycle_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/lifecycle"
)

// TestWave16_Lifecycle_Start_UnhealthyEmptyErrorFails covers the health-gate
// slip: an unhealthy health result carrying no error message (Healthy=false,
// Error="") was NOT treated as a failure because the gate also required
// result.Error != "". The service was then marked "running" and Start returned
// nil — a fabricated success (launch != working, §11.4.108-class bluff). The
// fix fails the start whenever the result is unhealthy, regardless of message.
func TestWave16_Lifecycle_Start_UnhealthyEmptyErrorFails(t *testing.T) {
	orch := &stubOrchestrator{}
	hc := &stubHealthChecker{healthy: false, errMsg: ""} // unhealthy, no message
	m := lifecycle.NewDefaultManager(orch, hc)

	require.NoError(t, m.Register(lifecycle.ServiceSpec{
		Name:        "svc",
		ComposeFile: "docker-compose.yml",
	}))

	err := m.Start(context.Background(), "svc")
	require.Error(t, err,
		"an unhealthy service must fail Start even with an empty error message")

	st, statErr := m.Status("svc")
	require.NoError(t, statErr)
	assert.Equal(t, "stopped", st.State,
		"an unhealthy service must not be left marked running")
	assert.False(t, st.Healthy)
}
