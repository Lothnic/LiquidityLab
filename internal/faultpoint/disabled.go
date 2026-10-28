//go:build !liquiditylab_faults

// Package faultpoint exposes inert checkpoints in ordinary builds. Only an
// explicitly tagged test binary can block on a test controller's private pipes.
package faultpoint

func Hit(string)  {}
func Note(string) {}
