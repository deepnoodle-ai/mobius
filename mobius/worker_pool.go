package mobius

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// WorkerPoolConfig configures an in-process pool of single-job workers.
type WorkerPoolConfig struct {
	WorkerConfig
	// Count is the number of single-job workers to run. Defaults to 1.
	Count int
	// WorkerIDPrefix is used to derive child worker IDs as
	// "<prefix>-<index>". When empty, the SDK generates a per-boot prefix.
	WorkerIDPrefix string
}

// WorkerPool runs multiple single-job workers in one process.
type WorkerPool struct {
	client   *Client
	config   WorkerPoolConfig
	registry *ActionRegistry
}

// NewWorkerPool creates a pool of single-job workers bound to the client.
func (c *Client) NewWorkerPool(cfg WorkerPoolConfig) *WorkerPool {
	if cfg.Count <= 0 {
		cfg.Count = 1
	}
	if cfg.WorkerIDPrefix == "" {
		cfg.WorkerIDPrefix = "worker-" + uuid.NewString()
	}
	cfg.WorkerID = ""
	return &WorkerPool{
		client:   c,
		config:   cfg,
		registry: NewActionRegistry(),
	}
}

// Register attaches a pre-constructed Action to every worker in the pool.
func (p *WorkerPool) Register(a Action) {
	p.registry.MustRegister(a)
}

// MustRegister attaches a pre-constructed Action to every worker in the pool.
func (p *WorkerPool) MustRegister(a Action) {
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
		cfg.WorkerID = fmt.Sprintf("%s-%d", p.config.WorkerIDPrefix, i)
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
