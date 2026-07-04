package config

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMissingKeysFullConfigHasNone(t *testing.T) {
	def := &Config{}
	applyDefaults(def)
	full, err := yaml.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := MissingKeys(full)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("a fully-defaulted config should have no missing keys, got %v", missing)
	}
}

func TestMissingKeysOldConfig(t *testing.T) {
	old := []byte(`vm:
  name: klimax
  cpus: 4
network:
  kindBridgeCIDR: "172.30.0.0/16"
`)
	missing, err := MissingKeys(old)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"vm.memory", "kind", "registries"} {
		if !slices.Contains(missing, want) {
			t.Errorf("expected %q among missing keys, got %v", want, missing)
		}
	}
}
