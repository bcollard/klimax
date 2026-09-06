package cli

import (
	"testing"

	"github.com/bcollard/klimax/internal/fleet"
	"github.com/bcollard/klimax/internal/kind"
	"gopkg.in/yaml.v3"
)

func info(version string, labels map[string]string) *kind.ClusterInfo {
	return &kind.ClusterInfo{KubeletVersion: version, Labels: labels}
}

func TestCustomLabelsDropsInfraAndManaged(t *testing.T) {
	got := customLabels(map[string]string{
		"kubernetes.io/hostname":           "dev-control-plane",
		"beta.kubernetes.io/arch":          "arm64",
		"node-role.kubernetes.io/control-plane": "",
		"node.kubernetes.io/exclude":       "true",
		"topology.kubernetes.io/region":    "europe-west1",
		"topology.kubernetes.io/zone":      "europe-west1-b",
		"managed-by":                       "klimax",
		"klimax.dev/fleet":                 "mesh",
		"env":                              "prod",
		"team":                             "platform",
	})
	want := map[string]string{"env": "prod", "team": "platform"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %q: got %q, want %q", k, got[k], v)
		}
	}
}

func TestCustomLabelsReturnsNilWhenOnlyInfra(t *testing.T) {
	// nil (not an empty map) keeps `labels:` out of the emitted YAML entirely.
	if got := customLabels(map[string]string{
		"kubernetes.io/hostname": "dev-control-plane",
		"managed-by":             "klimax",
	}); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestDeriveFleetName(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		infos map[string]*kind.ClusterInfo
		want  string
	}{
		{
			name:  "all share a fleet label",
			names: []string{"a", "b"},
			infos: map[string]*kind.ClusterInfo{
				"a": info("v1.36.1", map[string]string{"klimax.dev/fleet": "mesh"}),
				"b": info("v1.36.1", map[string]string{"klimax.dev/fleet": "mesh"}),
			},
			want: "mesh",
		},
		{
			name:  "mixed fleets fall back",
			names: []string{"a", "b"},
			infos: map[string]*kind.ClusterInfo{
				"a": info("v1.36.1", map[string]string{"klimax.dev/fleet": "mesh"}),
				"b": info("v1.36.1", map[string]string{"klimax.dev/fleet": "gw"}),
			},
			want: "exported",
		},
		{
			name:  "one unlabelled falls back",
			names: []string{"a", "b"},
			infos: map[string]*kind.ClusterInfo{
				"a": info("v1.36.1", map[string]string{"klimax.dev/fleet": "mesh"}),
				"b": info("v1.36.1", map[string]string{}),
			},
			want: "exported",
		},
		{
			name:  "unreachable cluster falls back",
			names: []string{"a"},
			infos: map[string]*kind.ClusterInfo{"a": nil},
			want:  "exported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveFleetName(tt.names, tt.infos); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAssembleFleetCapturesLiveState(t *testing.T) {
	names := []string{"dev", "staging"}
	nums := map[string]int{"dev": 1, "staging": 2}
	infos := map[string]*kind.ClusterInfo{
		"dev": info("v1.36.1", map[string]string{
			"topology.kubernetes.io/region": "europe-west1",
			"topology.kubernetes.io/zone":   "europe-west1-b",
			"klimax.dev/fleet":              "mesh",
			"kubernetes.io/hostname":        "dev-control-plane",
			"env":                           "dev",
		}),
		"staging": info("v1.36.1", map[string]string{
			"klimax.dev/fleet": "mesh",
		}),
	}

	man := assembleFleet(names, nums, infos, "", true)

	if man.APIVersion != fleet.APIVersion || man.Kind != fleet.Kind {
		t.Errorf("bad manifest header: %s / %s", man.APIVersion, man.Kind)
	}
	if man.Metadata.Name != "mesh" {
		t.Errorf("metadata.name: got %q, want %q (shared fleet label)", man.Metadata.Name, "mesh")
	}
	if len(man.Spec.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(man.Spec.Clusters))
	}

	dev := man.Spec.Clusters[0]
	if dev.Name != "dev" || dev.Num != 1 || dev.NodeVersion != "v1.36.1" {
		t.Errorf("dev entry: %+v", dev)
	}
	if dev.Region != "europe-west1" || dev.Zone != "europe-west1-b" {
		t.Errorf("dev topology: region=%q zone=%q", dev.Region, dev.Zone)
	}
	if dev.Labels["env"] != "dev" {
		t.Errorf("dev labels: %v", dev.Labels)
	}
	// The fleet label is reconstructed from metadata.name, so it must not also
	// appear as a free-form label — that would double-apply it.
	if _, dup := dev.Labels[fleetLabelKey]; dup {
		t.Errorf("fleet label leaked into entry labels: %v", dev.Labels)
	}

	if staging := man.Spec.Clusters[1]; staging.Labels != nil {
		t.Errorf("staging should have no custom labels, got %v", staging.Labels)
	}
}

func TestAssembleFleetExplicitNameAndNoNums(t *testing.T) {
	infos := map[string]*kind.ClusterInfo{
		"a": info("v1.36.1", map[string]string{"klimax.dev/fleet": "mesh"}),
	}
	man := assembleFleet([]string{"a"}, map[string]int{"a": 7}, infos, "chosen", false)
	if man.Metadata.Name != "chosen" {
		t.Errorf("explicit --name should win, got %q", man.Metadata.Name)
	}
	if man.Spec.Clusters[0].Num != 0 {
		t.Errorf("--nums=false should leave num unset, got %d", man.Spec.Clusters[0].Num)
	}
}

// The whole point of export is that apply can read the result back, so round-trip
// through the real parser rather than asserting on the struct alone.
func TestAssembleFleetRoundTripsThroughParse(t *testing.T) {
	infos := map[string]*kind.ClusterInfo{
		"dev": info("v1.36.1", map[string]string{
			"topology.kubernetes.io/region": "europe-west1",
			"klimax.dev/fleet":              "mesh",
			"env":                           "dev",
		}),
	}
	man := assembleFleet([]string{"dev"}, map[string]int{"dev": 3}, infos, "", true)

	out, err := yaml.Marshal(man)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := fleet.Parse(out)
	if err != nil {
		t.Fatalf("exported manifest does not parse: %v\n%s", err, out)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("exported manifest does not validate: %v\n%s", err, out)
	}
	if len(parsed.Spec.Clusters) != 1 || parsed.Spec.Clusters[0].Name != "dev" {
		t.Fatalf("round-trip lost the cluster: %+v", parsed.Spec.Clusters)
	}
	if parsed.Spec.Clusters[0].Num != 3 {
		t.Errorf("round-trip lost num: got %d, want 3", parsed.Spec.Clusters[0].Num)
	}
	if parsed.Metadata.Name != "mesh" {
		t.Errorf("round-trip lost fleet name: got %q", parsed.Metadata.Name)
	}
}
