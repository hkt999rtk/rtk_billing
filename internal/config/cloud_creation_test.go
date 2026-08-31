package config

import (
	"strings"
	"testing"
)

func TestCloudCreationCredentialIsOptionalAndSeparate(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("BILLING_CLOUD_CREATION_TOKEN", "")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BILLING_HANDOFF_TOKEN", strings.Repeat("o", 32))
	for _, token := range []string{"short", strings.Repeat("n", 32) + " x", strings.Repeat("s", 32), strings.Repeat("i", 32), strings.Repeat("d", 32), strings.Repeat("o", 32), strings.Repeat("h", 32), strings.Repeat("c", 32)} {
		t.Setenv("BILLING_CLOUD_CREATION_TOKEN", token)
		if _, err := Load(); err == nil {
			t.Fatal("reused credential")
		}
	}
	t.Setenv("BILLING_CLOUD_CREATION_TOKEN", strings.Repeat("n", 32))
	if cfg, err := Load(); err != nil || cfg.CloudCreationToken != strings.Repeat("n", 32) {
		t.Fatal(err)
	}
}
