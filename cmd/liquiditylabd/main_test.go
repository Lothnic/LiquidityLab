package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalListenerBoundary(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "192.0.2.1:0", "localhost:0", "unix:relative.sock"} {
		listener, cleanup, err := localListener(address)
		if err == nil {
			cleanup()
			t.Fatalf("accepted nonlocal or ambiguous endpoint %s (%v)", address, listener.Addr())
		}
	}
	listener, cleanup, err := localListener("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if listener.Addr() == nil {
		t.Fatal("missing loopback address")
	}
	cleanup()
	path := filepath.Join(t.TempDir(), "replay.sock")
	listener, cleanup, err = localListener("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v", info.Mode())
	}
	_, _, err = localListener("unix:" + path)
	if err == nil {
		t.Fatal("overwrote active socket")
	}
	cleanup()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("owned Unix socket not removed")
	}
}
