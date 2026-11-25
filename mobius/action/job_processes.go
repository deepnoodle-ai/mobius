package action

import (
	"sync"
	"time"
)

// JobProcesses records the root process of each running environment.bash
// command and the lease of the job that started it. The worker's action
// socket uses it to find which job a calling process belongs to: it walks
// the caller's parent processes until it reaches one of these roots. The
// lease never has to enter the command's environment for that.
var JobProcesses = &jobProcesses{byPID: map[int]string{}}

type jobProcesses struct {
	mu    sync.RWMutex
	byPID map[int]string
	// starting counts commands between their start and their Register. A
	// command's first child can call the socket in that window.
	starting int
}

// Starting marks a job command that is about to start. Call the returned
// func once the command is registered, or when it fails to start. Extra
// calls do nothing.
func (r *jobProcesses) Starting() func() {
	r.mu.Lock()
	r.starting++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.starting--
			r.mu.Unlock()
		})
	}
}

// WaitStarts waits up to timeout for every command marked by Starting to be
// registered. It reports whether any start was in progress when it was
// called, so a caller knows a second lookup can find a new registration.
func (r *jobProcesses) WaitStarts(timeout time.Duration) bool {
	r.mu.RLock()
	pending := r.starting > 0
	r.mu.RUnlock()
	if !pending {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		r.mu.RLock()
		pending = r.starting > 0
		r.mu.RUnlock()
		if !pending {
			break
		}
	}
	return true
}

// Register records pid as the root of a job's command. The returned func
// removes it; call it when the command ends.
func (r *jobProcesses) Register(pid int, leaseToken string) func() {
	if pid <= 0 || leaseToken == "" {
		return func() {}
	}
	r.mu.Lock()
	r.byPID[pid] = leaseToken
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.byPID[pid] == leaseToken {
			delete(r.byPID, pid)
		}
		r.mu.Unlock()
	}
}

// Lease returns the lease of the job whose command root is pid.
func (r *jobProcesses) Lease(pid int) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	lease, ok := r.byPID[pid]
	return lease, ok
}
