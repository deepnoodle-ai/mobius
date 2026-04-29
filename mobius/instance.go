package mobius

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
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

// bootInstanceID is generated once per process and reused by both the
// system_hostname rung (as an 8-char suffix) and the generated_uuid
// rung (full value). worker_instance_id is process identity and must
// be stable across calls within the same boot — without the cache,
// a caller that resolved twice would observe two different IDs.
var bootInstanceID = sync.OnceValue(uuid.NewString)

// ResolveInstanceID derives a per-process worker_instance_id from the
// runtime environment. Resolution order:
//
//  1. explicit (caller-configured value)
//  2. K_REVISION + Cloud Run instance metadata
//  3. HOSTNAME env (Kubernetes pod, Docker container — exported by bash)
//  4. FLY_MACHINE_ID
//  5. RAILWAY_REPLICA_ID
//  6. RENDER_INSTANCE_ID
//  7. system hostname via os.Hostname() suffixed with a per-boot
//     random tag (laptops, dev VMs, bare metal)
//  8. generated UUID (per-process boot, last resort)
//
// The system_hostname rung carries a random suffix because os.Hostname()
// identifies the host, not the process — two processes started on the
// same machine (back-to-back tests, parallel CI workers, dev box with
// a daemon already running) would otherwise auto-detect the same
// worker_instance_id and trip the server's conflict detector. Operators
// who want a stable identity across restarts (named singleton workers)
// should set [WorkerConfig.WorkerInstanceID] explicitly.
//
// The returned source is informational only — workers log it once at
// startup so operators can confirm the right platform was picked up.
func ResolveInstanceID(explicit string) (string, InstanceIDSource) {
	if trimmed := strings.TrimSpace(explicit); trimmed != "" {
		return trimmed, InstanceIDSourceConfigured
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
			return h + "-" + bootInstanceID()[:8], InstanceIDSourceSystemHostname
		}
	}
	return bootInstanceID(), InstanceIDSourceGeneratedUUID
}

// cloudRunInstanceID hits the GCE metadata server for the per-instance
// ID and combines it with K_REVISION so the same revision rolling out
// across replicas yields one row per replica. Returns empty when not
// running on Cloud Run or the probe fails — falling through to the
// next strategy (HOSTNAME, which Cloud Run also sets per-instance)
// rather than returning a bare revision string that would collapse
// every replica onto the same row and trip the conflict detector.
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
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(body))
	if id == "" {
		return ""
	}
	return revision + "-" + id
}
