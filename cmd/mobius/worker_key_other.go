//go:build !linux

package main

// protectWorkerProcess does nothing off Linux. Managed environments run
// Linux; a worker on another system relies on the variable being removed.
func protectWorkerProcess() {}
