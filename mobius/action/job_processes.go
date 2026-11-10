package action

import "sync"

// JobProcesses records the root process of each running environment.bash
// command and the lease of the job that started it. The worker's action
// socket uses it to find which job a calling process belongs to: it walks
// the caller's parent processes until it reaches one of these roots. The
// lease never has to enter the command's environment for that.
var JobProcesses = &jobProcesses{byPID: map[int]string{}}

type jobProcesses struct {
	mu    sync.RWMutex
	byPID map[int]string
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
