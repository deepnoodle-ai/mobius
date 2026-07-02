package mobius

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
	"github.com/gorilla/websocket"
)

// TestWorkerRun_ResendsUnackedReportAfterReconnect is the regression for
// write-and-forget report delivery: a report whose write succeeded but whose
// connection died before the server processed it must be re-sent on the next
// connection, and only a job.report.ack may clear it.
func TestWorkerRun_ResendsUnackedReportAfterReconnect(t *testing.T) {
	var conns atomic.Int32
	redelivered := make(chan api.WorkerSocketJobReportFrame, 1)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })

	c, _ := newWorkerSocketTestClient(t, func(t *testing.T, conn *websocket.Conn) {
		switch conns.Add(1) {
		case 1:
			expectRegister(t, conn, "quick", nil)
			expectClaim(t, conn)
			sendClaimed(t, conn, api.WorkerSocketClaimedJob{
				Id:                      "job_ack_1",
				Kind:                    api.WorkerSocketClaimedJobKindActionExecution,
				Origin:                  api.WorkerSocketClaimedJobOriginLoopActionStep,
				ExecutorKind:            api.WorkerSocketClaimedJobExecutorKindCustomerWorker,
				ActionName:              strPtr("quick"),
				Queue:                   "default",
				RunId:                   strPtr("run_1"),
				ClaimAttempt:            1,
				LeaseToken:              "lease-ack-1",
				HeartbeatCadenceSeconds: 60,
				LeaseDurationSeconds:    180,
				Spec:                    map[string]any{"parameters": map[string]any{}},
			})
			// Receive the terminal report but never acknowledge it, then drop
			// the connection — the exact window in which a written-but-
			// unprocessed report used to be silently lost.
			for {
				if readTestFrame(t, conn).Type == "job.report" {
					return
				}
			}
		default:
			expectRegister(t, conn, "quick", nil)
			for {
				env := readTestFrame(t, conn)
				if env.Type != "job.report" {
					continue
				}
				var report api.WorkerSocketJobReportFrame
				assert.NoError(t, json.Unmarshal(env.Raw, &report))
				_ = conn.WriteJSON(api.WorkerSocketJobReportAckFrame{
					Type:      api.WorkerSocketJobReportAckFrameTypeJobReportAck,
					JobId:     report.JobId,
					MessageId: report.MessageId,
				})
				select {
				case redelivered <- report:
				default:
				}
				<-hold
				return
			}
		}
	})

	worker := c.NewWorker(WorkerConfig{
		WorkerInstanceID:  "w-ack",
		ReconnectDelay:    10 * time.Millisecond,
		HeartbeatInterval: time.Hour,
	})
	worker.Register(ActionFunc("quick", func(ctx Context, params map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case report := <-redelivered:
		assert.Equal(t, report.JobId, "job_ack_1")
		assert.Equal(t, report.LeaseToken, "lease-ack-1")
	case <-time.After(2 * time.Second):
		t.Fatal("unacked report was not re-sent after reconnect")
	}
	// The ack must clear the pending report — otherwise it would be re-sent
	// forever.
	waitFor(t, func() bool {
		worker.mu.Lock()
		defer worker.mu.Unlock()
		return len(worker.pendingReports) == 0
	}, "job.report.ack to clear the pending report")
	cancel()
	<-done
}

// TestWorkerRun_ShutdownDeliversCancelledReport is the regression for
// shutdown dropping every in-flight job's terminal report: cancelling the
// worker must cancel the job AND deliver its `cancelled` report over the
// still-open socket before it closes, so the server doesn't have to
// rediscover the job's fate via lease expiry.
func TestWorkerRun_ShutdownDeliversCancelledReport(t *testing.T) {
	reported := make(chan api.WorkerSocketJobReportFrame, 1)
	started := make(chan struct{})

	c, _ := newWorkerSocketTestClient(t, func(t *testing.T, conn *websocket.Conn) {
		expectRegister(t, conn, "block-shutdown", nil)
		expectClaim(t, conn)
		sendClaimed(t, conn, api.WorkerSocketClaimedJob{
			Id:                      "job_shutdown_1",
			Kind:                    api.WorkerSocketClaimedJobKindActionExecution,
			Origin:                  api.WorkerSocketClaimedJobOriginLoopActionStep,
			ExecutorKind:            api.WorkerSocketClaimedJobExecutorKindCustomerWorker,
			ActionName:              strPtr("block-shutdown"),
			Queue:                   "default",
			RunId:                   strPtr("run_1"),
			ClaimAttempt:            1,
			LeaseToken:              "lease-shutdown-1",
			HeartbeatCadenceSeconds: 60,
			LeaseDurationSeconds:    180,
			Spec:                    map[string]any{"parameters": map[string]any{}},
		})
		for {
			env := readTestFrame(t, conn)
			if env.Type != "job.report" {
				continue
			}
			var report api.WorkerSocketJobReportFrame
			assert.NoError(t, json.Unmarshal(env.Raw, &report))
			reported <- report
			return
		}
	})

	worker := c.NewWorker(WorkerConfig{
		WorkerInstanceID:  "w-shutdown",
		ReconnectDelay:    10 * time.Millisecond,
		HeartbeatInterval: time.Hour,
	})
	var startedOnce atomic.Bool
	worker.Register(ActionFunc("block-shutdown", func(ctx Context, params map[string]any) (any, error) {
		if startedOnce.CompareAndSwap(false, true) {
			close(started)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not start")
	}
	cancel() // worker shutdown with the job mid-flight

	select {
	case report := <-reported:
		assert.Equal(t, report.JobId, "job_shutdown_1")
		assert.NotNil(t, report.Status)
		assert.Equal(t, *report.Status, api.WorkerSocketJobReportFrameStatusCancelled)
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown dropped the in-flight job's terminal report")
	}
	<-done
}

// TestWorkerRun_OverCapacityClaimedJobIsReported is the regression for
// silently dropping claimed jobs when the server returns more than the free
// slots: the excess job must be failed back immediately (so the server can
// requeue it in seconds) rather than ghosted until its lease expires.
func TestWorkerRun_OverCapacityClaimedJobIsReported(t *testing.T) {
	overCapacity := make(chan api.WorkerSocketJobReportFrame, 2)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	newJob := func(id string) api.WorkerSocketClaimedJob {
		return api.WorkerSocketClaimedJob{
			Id:                      id,
			Kind:                    api.WorkerSocketClaimedJobKindActionExecution,
			Origin:                  api.WorkerSocketClaimedJobOriginLoopActionStep,
			ExecutorKind:            api.WorkerSocketClaimedJobExecutorKindCustomerWorker,
			ActionName:              strPtr("block-capacity"),
			Queue:                   "default",
			RunId:                   strPtr("run_1"),
			ClaimAttempt:            1,
			LeaseToken:              "lease-" + id,
			HeartbeatCadenceSeconds: 60,
			LeaseDurationSeconds:    180,
			Spec:                    map[string]any{"parameters": map[string]any{}},
		}
	}

	c, _ := newWorkerSocketTestClient(t, func(t *testing.T, conn *websocket.Conn) {
		expectRegister(t, conn, "block-capacity", nil)
		claim := expectClaim(t, conn)
		assert.Equal(t, *claim.AvailableSlots, 1)
		// Overfill: two jobs against one slot.
		sendClaimed(t, conn, newJob("job_cap_1"), newJob("job_cap_2"))
		for {
			env := readTestFrame(t, conn)
			if env.Type != "job.report" {
				continue
			}
			var report api.WorkerSocketJobReportFrame
			assert.NoError(t, json.Unmarshal(env.Raw, &report))
			overCapacity <- report
			return
		}
	})

	worker := c.NewWorker(WorkerConfig{
		WorkerInstanceID:  "w-capacity",
		Concurrency:       1,
		ReconnectDelay:    10 * time.Millisecond,
		HeartbeatInterval: time.Hour,
	})
	worker.Register(ActionFunc("block-capacity", func(ctx Context, params map[string]any) (any, error) {
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case report := <-overCapacity:
		// Job 1 holds the only slot (it blocks on release), so the first — and
		// only immediate — report must be the overflow rejection for job 2.
		assert.Equal(t, report.JobId, "job_cap_2")
		assert.NotNil(t, report.Status)
		assert.Equal(t, *report.Status, api.WorkerSocketJobReportFrameStatusFailed)
		assert.Equal(t, *report.ErrorType, "WorkerOverCapacity")
	case <-time.After(2 * time.Second):
		t.Fatal("over-capacity claimed job was dropped without a report")
	}
	cancel()
	<-done
}

// TestWorkerRegister_UnblocksOnContextCancel is the regression for the
// unbounded registration read: a server that accepts the upgrade but never
// answers worker.register must not hang the worker past its context.
func TestWorkerRegister_UnblocksOnContextCancel(t *testing.T) {
	registerSeen := make(chan struct{})
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })

	c, _ := newWorkerSocketTestClient(t, func(t *testing.T, conn *websocket.Conn) {
		env := readTestFrame(t, conn)
		assert.Equal(t, env.Type, "worker.register")
		close(registerSeen)
		<-hold // never answer
	})

	worker := c.NewWorker(WorkerConfig{
		WorkerInstanceID:  "w-register-hang",
		HeartbeatInterval: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.runSocket(ctx) }()

	select {
	case <-registerSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never sent worker.register")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("registration read did not unblock on context cancel")
	}
}
