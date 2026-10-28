//go:build liquiditylab_faults

package faultpoint

import (
	"io"
	"os"
	"sync"
)

var once sync.Once
var write sync.Mutex
var events, release *os.File

func init() {
	if os.Getenv("LIQUIDITYLAB_TEST_FAULT_POINT") != "" {
		events = os.NewFile(3, "fault-events")
		release = os.NewFile(4, "fault-release")
	}
}

// Hit is active only in the explicit test build. The controller supplies FD 3
// for bounded event names and FD 4 for a release byte; disconnect fails closed.
func Hit(name string) {
	if name != os.Getenv("LIQUIDITYLAB_TEST_FAULT_POINT") {
		return
	}
	once.Do(func() {
		Note("reached:" + name)
		var token [1]byte
		if _, err := io.ReadFull(release, token[:]); err != nil {
			os.Exit(86)
		}
		Note("released:" + name)
	})
}

func Note(name string) {
	if os.Getenv("LIQUIDITYLAB_TEST_FAULT_POINT") == "" {
		return
	}
	write.Lock()
	defer write.Unlock()
	if _, err := io.WriteString(events, name+"\n"); err != nil {
		os.Exit(86)
	}
}
