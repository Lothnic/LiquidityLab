//go:build !liquiditylab_faults

package faultpoint

import "testing"

func TestOrdinaryBuildIgnoresFaultControllerSettings(t *testing.T) {
	t.Setenv("LIQUIDITYLAB_TEST_FAULT_POINT", "intent_committed")
	// No inherited pipes exist. An ordinary build must neither block nor inspect
	// test controller descriptors, even if the environment variable is present.
	Hit("intent_committed")
	Note("handoff_returned")
}
