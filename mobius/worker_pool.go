package mobius

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// WorkerPoolConfig configures an in-process pool of worker instances.
//
// Most callers do not need a pool. To run several jobs from one
// process, set [WorkerConfig.Concurrency] on a single [Worker]; the
// admin UI shows one row with a saturation bar. Reach for a pool
// only when you need each child to surface as its own row on the
// workers page — for example when child workers should drain
// independently or when isolation between in-flight jobs matters.
type WorkerPoolConfig struct {
	WorkerConfig
	// Count is the number of worker instances to run. Defaults to 1.
	Count int
	// WorkerInstanceIDPrefix is used to derive child instance IDs as
	// "<prefix>-<index>". When empty, the SDK generates a per-boot
	// prefix; child workers each get their own session token, so a
	// pool of N produces N rows on the workers page.
	WorkerInstanceIDPrefix string
}

// WorkerPool runs multiple worker instances in one process. Prefer
// [WorkerConfig.Concurrency] on a single [Worker] for raw throughput.
type WorkerPool struct {
	client   *Client
	config   WorkerPoolConfig
	registry *ActionRegistry
}

// NewWorkerPool creates a pool of worker instances bound to the client.
func (c *Client) NewWorkerPool(cfg WorkerPoolConfig) *WorkerPool {
	if cfg.Count <= 0 {
		cfg.Count = 1
	}
	if cfg.WorkerInstanceIDPrefix == "" {
		cfg.WorkerInstanceIDPrefix = "worker-" + uuid.NewString()
	}
	cfg.WorkerInstanceID = ""
	return &WorkerPool{
		client:   c,
		config:   cfg,
		registry: NewActionRegistry(),
	}
}

// Register attaches a pre-constructed Action to every worker in the pool.
// It panics on invalid setup, matching Worker.Register.
func (p *WorkerPool) Register(a Action) {
	p.registry.MustRegister(a)
}

// Run starts every worker in the pool and blocks until ctx is cancelled or
// any worker observes credential revocation.
func (p *WorkerPool) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, p.config.Count)
	var wg sync.WaitGroup
	for i := 1; i <= p.config.Count; i++ {
		cfg := p.config.WorkerConfig
		cfg.WorkerInstanceID = fmt.Sprintf("%s-%d", p.config.WorkerInstanceIDPrefix, i)
		worker := p.client.newWorkerWithRegistry(cfg, p.registry)

		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- worker.Run(runCtx)
		}()
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	var result error
	for err := range errCh {
		switch {
		case err == nil:
		case errors.Is(err, ErrAuthRevoked):
			result = ErrAuthRevoked
			cancel()
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			if result == nil {
				result = err
			}
		default:
			if result == nil {
				result = err
				cancel()
			}
		}
	}
	return result
}
