package mobius

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// InstanceIDSource describes which environment input produced the
// auto-detected worker_instance_id. Surfaced via the result of
// [ResolveInstanceID] so worker startup can log it for operator
// debugging — "did this process pick up its Cloud Run revision, or
// did it fall back to a UUID?"
type InstanceIDSource string

const (
	InstanceIDSourceConfigured       InstanceIDSource = "configured"
	InstanceIDSourceCloudRunRevision InstanceIDSource = "cloud_run_revision_instance"
	InstanceIDSourceHostname         InstanceIDSource = "hostname"
	InstanceIDSourceFlyMachine       InstanceIDSource = "fly_machine_id"
	InstanceIDSourceRailwayReplica   InstanceIDSource = "railway_replica_id"
	InstanceIDSourceRenderInstance   InstanceIDSource = "render_instance_id"
	InstanceIDSourceSystemHostname   InstanceIDSource = "system_hostname"
	InstanceIDSourceGeneratedUUID    InstanceIDSource = "generated_uuid"
)

// cloudRunMetadataTimeout caps the metadata-server probe so a
// non-Cloud-Run host doesn't pay a full TCP timeout on startup.
const cloudRunMetadataTimeout = time.Second

// ResolveInstanceID derives a stable per-process worker_instance_id
// from the runtime environment. Resolution order:
//
//  1. explicit (caller-configured value)
//  2. K_REVISION + Cloud Run instance metadata
//  3. HOSTNAME env (Kubernetes pod, Docker container — exported by bash)
//  4. FLY_MACHINE_ID
//  5. RAILWAY_REPLICA_ID
//  6. RENDER_INSTANCE_ID
//  7. system hostname via os.Hostname() (laptops, dev VMs, bare metal)
//  8. generated UUID (per-process boot, last resort)
//
// The returned source is informational only — workers log it once at
// startup so operators can confirm the right platform was picked up.
func ResolveInstanceID(explicit string) (string, InstanceIDSource) {
	if explicit != "" {
		return explicit, InstanceIDSourceConfigured
	}
	if id := cloudRunInstanceID(context.Background()); id != "" {
		return id, InstanceIDSourceCloudRunRevision
	}
	if id := strings.TrimSpace(os.Getenv("HOSTNAME")); id != "" {
		return id, InstanceIDSourceHostname
	}
	if id := strings.TrimSpace(os.Getenv("FLY_MACHINE_ID")); id != "" {
		return id, InstanceIDSourceFlyMachine
	}
	if id := strings.TrimSpace(os.Getenv("RAILWAY_REPLICA_ID")); id != "" {
		return id, InstanceIDSourceRailwayReplica
	}
	if id := strings.TrimSpace(os.Getenv("RENDER_INSTANCE_ID")); id != "" {
		return id, InstanceIDSourceRenderInstance
	}
	if host, err := os.Hostname(); err == nil {
		if h := strings.TrimSpace(host); h != "" {
			return h, InstanceIDSourceSystemHostname
		}
	}
	return uuid.NewString(), InstanceIDSourceGeneratedUUID
}

// cloudRunInstanceID hits the GCE metadata server for the per-instance
// ID and combines it with K_REVISION so the same revision rolling out
// across replicas yields one row per replica. Returns empty when not
// running on Cloud Run or the probe times out.
func cloudRunInstanceID(ctx context.Context) string {
	revision := strings.TrimSpace(os.Getenv("K_REVISION"))
	if revision == "" {
		return ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, cloudRunMetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/id", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return revision
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return revision
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return revision
	}
	id := strings.TrimSpace(string(body))
	if id == "" {
		return revision
	}
	return revision + "-" + id
}
