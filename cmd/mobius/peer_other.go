//go:build !linux && !darwin

package main

import (
	"errors"
	"net"
)

// errNoPeerCheck refuses action calls where the worker cannot tell which
// process is calling.
var errNoPeerCheck = errors.New("this worker cannot check which process is calling, so it refuses action calls")

func peerPID(net.Conn) (int, error) { return 0, errNoPeerCheck }

func parentPID(int) (int, error) { return 0, errNoPeerCheck }
