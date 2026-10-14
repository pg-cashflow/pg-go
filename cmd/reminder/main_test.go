package main

import "testing"

// Fail-closed, -help, and invalid-flag checks are canonical in
// internal/jobs TestGate12_BinariesMissingEnvFailFast and TestGate12_HelpFlagAndInvalidFlag.
func TestCLIContractDelegatedToGate12(t *testing.T) {
	if testing.Short() {
		t.Skip("Gate12 covers this binary")
	}
}
