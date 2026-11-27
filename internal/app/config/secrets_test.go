package config

import "testing"

func TestSecretVaultNilConfigGuards(t *testing.T) {
	if got := ConfiguredSecrets(nil); got != nil {
		t.Fatalf("ConfiguredSecrets(nil) = %v, want nil", got)
	}
	if got := AdditionalConfiguredSecrets(nil); got != nil {
		t.Fatalf("AdditionalConfiguredSecrets(nil) = %v, want nil", got)
	}
	if vault, err := BuildStrictSecretVault(nil); err != nil || vault == nil {
		t.Fatalf("BuildStrictSecretVault(nil) = (%v, %v), want non-nil vault and nil error", vault, err)
	}
	if vault := BuildSecretVault(nil); vault == nil {
		t.Fatal("BuildSecretVault(nil) = nil, want non-nil vault")
	}
}
