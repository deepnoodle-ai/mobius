//go:build !linux && !darwin

package main

import "net"

func peerPID(net.Conn) (int, error) { return 0, errNoPeerCheck }

func parentPID(int) (int, error) { return 0, errNoPeerCheck }
