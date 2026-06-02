package compose

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHelixComposeProject(t *testing.T) {
	services := DefaultHelixServices()
	p := NewHelixComposeProject("helix-test", services)

	require.NotNil(t, p)
	assert.Equal(t, "helix-test", p.ProjectName)
	assert.Len(t, p.Services, 20)
	assert.Contains(t, p.Networks, "helix")
}

func TestHelixComposeProject_GetService(t *testing.T) {
	p := NewHelixComposeProject("test", DefaultHelixServices())

	s, err := p.GetService("postgres-primary")
	require.NoError(t, err)
	assert.Equal(t, "postgres:16-alpine", s.Image)
	assert.NotNil(t, s.HealthCheck)

	_, err = p.GetService("nonexistent")
	assert.Error(t, err)
}

func TestHelixComposeProject_ServiceNames(t *testing.T) {
	p := NewHelixComposeProject("test", DefaultHelixServices())
	names := p.ServiceNames()
	assert.Len(t, names, 20)
	assert.Contains(t, names, "postgres-primary")
	assert.Contains(t, names, "vault")
}

func TestHelixComposeProject_HasService(t *testing.T) {
	p := NewHelixComposeProject("test", DefaultHelixServices())
	assert.True(t, p.HasService("nats"))
	assert.False(t, p.HasService("nonexistent"))
}

func TestDefaultHelixServices_Count(t *testing.T) {
	services := DefaultHelixServices()
	assert.Len(t, services, 20)
}

func TestDefaultHelixServices_PostgresHealthCheck(t *testing.T) {
	services := DefaultHelixServices()
	var pg *HelixService
	for i := range services {
		if services[i].Name == "postgres-primary" {
			pg = &services[i]
			break
		}
	}
	require.NotNil(t, pg)
	require.NotNil(t, pg.HealthCheck)
	assert.Equal(t, 5*time.Second, pg.HealthCheck.Interval)
	assert.Equal(t, 5, pg.HealthCheck.Retries)
}

// Paired mutation: break GetService to return wrong service
func TestHelixComposeProject_GetService_Mutation(t *testing.T) {
	p := NewHelixComposeProject("test", DefaultHelixServices())

	s, err := p.GetService("vault")
	require.NoError(t, err)
	assert.Equal(t, "vault", s.Name)
	assert.Equal(t, "hashicorp/vault:1.16", s.Image)

	s2, err := p.GetService("postgres-primary")
	require.NoError(t, err)
	assert.Equal(t, "postgres-primary", s2.Name)
	assert.NotEqual(t, s.Image, s2.Image, "different services should have different images")
}
