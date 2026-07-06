package config

import (
	"strings"
	"testing"
)

func baseValidConfig() *Config {
	cfg := &Config{}
	applyDefaults(cfg)
	return cfg
}

func TestValidateRejectsHostnameMirrorName(t *testing.T) {
	for _, bad := range []string{"quay.io", "registry-1.docker.io", "us-docker.pkg.dev", "host:5000"} {
		cfg := baseValidConfig()
		cfg.Registries.Mirrors = []RegistryMirror{
			{Name: bad, Port: 5010, RemoteURL: "https://quay.io"},
		}
		err := Validate(cfg)
		if err == nil {
			t.Errorf("Validate accepted hostname-like mirror name %q, want error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "hostname") {
			t.Errorf("mirror %q: error should mention hostname shadowing, got: %v", bad, err)
		}
	}
}

func TestValidateAcceptsSafeMirrorName(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Registries.Mirrors = []RegistryMirror{
		{Name: "registry-quayio", Port: 5010, RemoteURL: "https://quay.io"},
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("Validate rejected a safe mirror name: %v", err)
	}
}
