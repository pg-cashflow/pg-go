package main

import "testing"

func TestCLIContractDelegatedToGate12(t *testing.T) {
	if testing.Short() {
		t.Skip("Gate12 covers this binary")
	}
}
