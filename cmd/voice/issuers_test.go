package main

import (
	"slices"
	"testing"
)

// While the platform's issuer moves from hanzo.id to hanzo.ai the gate trusts both
// spellings, each exactly, and never an organization's own issuer.
func TestBothPlatformIssuersAreTrustedAndAnOrgsIsNot(t *testing.T) {
	for _, configured := range []string{"https://hanzo.id", "https://hanzo.ai"} {
		got := trustedIssuers(configured)
		for _, want := range []string{"https://hanzo.id", "https://hanzo.ai"} {
			if !slices.Contains(got, want) {
				t.Errorf("configured %s does not trust %s: %v", configured, want, got)
			}
		}
		if slices.Contains(got, "https://hanzo.ai/acme") {
			t.Errorf("configured %s trusts an organization's issuer: %v", configured, got)
		}
	}
	for _, configured := range []string{"https://hanzo.ai/acme", "https://elsewhere.example"} {
		if got := trustedIssuers(configured); len(got) != 1 || got[0] != configured {
			t.Errorf("configured %s trusts %v", configured, got)
		}
	}
}
