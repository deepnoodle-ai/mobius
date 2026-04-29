package mobius

import (
	"os"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/assert"
)

// TestResolveInstanceID_ExplicitWins covers the configured-value
// short-circuit so an operator override always beats auto-detection.
func TestResolveInstanceID_ExplicitWins(t *testing.T) {
	id, source := ResolveInstanceID("override")
	assert.Equal(t, id, "override")
	assert.Equal(t, source, InstanceIDSourceConfigured)
}

// TestResolveInstanceID_FallsBackToSystemHostname makes sure laptops
// and dev VMs (no platform env vars set) get a human-readable hostname
// prefix plus a per-boot random suffix, so two processes on the same
// host never auto-detect to the same worker_instance_id.
func TestResolveInstanceID_FallsBackToSystemHostname(t *testing.T) {
	t.Setenv("K_REVISION", "")
	t.Setenv("HOSTNAME", "")
	t.Setenv("FLY_MACHINE_ID", "")
	t.Setenv("RAILWAY_REPLICA_ID", "")
	t.Setenv("RENDER_INSTANCE_ID", "")

	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("os.Hostname() unavailable on this system; nothing to assert")
	}

	id, source := ResolveInstanceID("")
	assert.Equal(t, source, InstanceIDSourceSystemHostname)
	assert.True(t, strings.HasPrefix(id, host+"-"),
		"expected id %q to start with %q-", id, host)
	assert.Equal(t, len(id), len(host)+1+8)

	id2, _ := ResolveInstanceID("")
	assert.NotEqual(t, id, id2, "back-to-back resolutions must differ")
}

// TestResolveInstanceID_HostnameEnvBeatsSystemHostname confirms the
// K8s/Docker convention (HOSTNAME env exported by bash) still wins
// over the syscall-derived hostname — they're commonly the same value
// on those platforms but the env path is faster and explicit.
func TestResolveInstanceID_HostnameEnvBeatsSystemHostname(t *testing.T) {
	t.Setenv("K_REVISION", "")
	t.Setenv("HOSTNAME", "pod-abc123")
	t.Setenv("FLY_MACHINE_ID", "")
	t.Setenv("RAILWAY_REPLICA_ID", "")
	t.Setenv("RENDER_INSTANCE_ID", "")

	id, source := ResolveInstanceID("")
	assert.Equal(t, id, "pod-abc123")
	assert.Equal(t, source, InstanceIDSourceHostname)
}
