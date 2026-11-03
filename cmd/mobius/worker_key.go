package main

import "os"

// workerAPIKeyEnv is the variable that carries the worker's API key into the
// worker process.
const workerAPIKeyEnv = "MOBIUS_API_KEY"

// hideWorkerKeyFromChildren keeps the worker's API key away from every process
// the worker starts: environment.bash, coding agents, git and its hooks, and
// anything those start. The key has already been read into the client.
//
// Removing the variable covers inherited environments. On Linux the process is
// also marked non-dumpable, because /proc/<pid>/environ keeps the environment
// the process started with, and a child running as the same user could
// otherwise read the key from its parent there. A child that execs a program
// is dumpable again, so this changes nothing for the children themselves.
//
// Scripts in a job that call the `mobius` CLI with the worker's key stop
// working. That is intended: the key is for the worker alone.
func hideWorkerKeyFromChildren() {
	_ = os.Unsetenv(workerAPIKeyEnv)
	protectWorkerProcess()
}
