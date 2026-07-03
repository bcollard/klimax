// Package clusterset parses and validates the klimax ClusterSet manifest — a
// declarative description of a fleet of kind clusters applied via
// `klimax cluster apply -f`. It is deliberately separate from the infrastructure
// config (~/.klimax/config.yaml): the manifest is an ephemeral input, like a
// `kubectl apply -f` object, and never lives in the config file.
//
// The minimal manifest a user must write only lists cluster names:
//
//	apiVersion: klimax.dev/v1alpha1
//	kind: ClusterSet
//	spec:
//	  clusters:
//	    - dev
//	    - staging
//
// Every other field (dependsOn, num, nodeVersion, registries, addons, defaults,
// maxParallel, strategy) is optional and falls back to sensible defaults.
package clusterset

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// APIVersion is the manifest apiVersion accepted by `klimax cluster apply`.
	APIVersion = "klimax.dev/v1alpha1"
	// Kind is the only manifest kind supported today.
	Kind = "ClusterSet"

	// StrategyFailFast stops scheduling new clusters after the first failure.
	StrategyFailFast = "FailFast"
	// StrategyContinueOnError keeps creating independent clusters after a failure.
	StrategyContinueOnError = "ContinueOnError"

	// MirrorsAll is the wildcard selector meaning "all configured mirrors".
	MirrorsAll = "*"
)

// clusterNameRE is a light sanity check; kind enforces the strict rules.
var clusterNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// ClusterSet is the top-level manifest.
type ClusterSet struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name string `yaml:"name"`
}

type Spec struct {
	// MaxParallel caps concurrent cluster creations. 0 or 1 means fully sequential.
	MaxParallel int `yaml:"maxParallel"`
	// Strategy is FailFast (default) or ContinueOnError.
	Strategy string `yaml:"strategy"`
	// Defaults are applied to every cluster that does not set the field itself.
	Defaults Defaults `yaml:"defaults"`
	// Clusters is the fleet. Each entry may be a bare name string or an object.
	Clusters []ClusterEntry `yaml:"clusters"`
}

// Defaults holds field values inherited by every cluster entry.
type Defaults struct {
	NodeVersion string          `yaml:"nodeVersion"`
	Region      string          `yaml:"region"`
	Zone        string          `yaml:"zone"`
	Registries  *RegistrySelect `yaml:"registries"`
	Addons      *Addons         `yaml:"addons"`
}

// ClusterEntry is one cluster in the set. It unmarshals from either a bare
// string (just the name) or a mapping with options.
type ClusterEntry struct {
	Name        string          `yaml:"name"`
	DependsOn   []string        `yaml:"dependsOn"`
	Num         int             `yaml:"num"`
	NodeVersion string          `yaml:"nodeVersion"`
	Region      string          `yaml:"region"`
	Zone        string          `yaml:"zone"`
	Registries  *RegistrySelect `yaml:"registries"`
	Addons      *Addons         `yaml:"addons"`
}

// UnmarshalYAML accepts either a scalar (the cluster name) or a full mapping.
// This is what lets the minimal manifest list only names.
func (c *ClusterEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&c.Name)
	}
	type raw ClusterEntry // avoid recursion into this method
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*c = ClusterEntry(r)
	return nil
}

// RegistrySelect cherry-picks which registries a cluster uses. A nil pointer
// means "inherit the default" (all configured registries + local registry).
type RegistrySelect struct {
	// LocalRegistry toggles the kind-registry push registry. nil = inherit (true).
	LocalRegistry *bool `yaml:"localRegistry"`
	// Mirrors selects pull-through mirrors by name. nil = all; ["*"] = all;
	// [] = none; [names...] = exactly those.
	Mirrors *[]string `yaml:"mirrors"`
}

// Addons is the extensible per-cluster addon set. Only metrics-server today.
type Addons struct {
	MetricsServer *MetricsServer `yaml:"metricsServer"`
}

type MetricsServer struct {
	Enabled bool `yaml:"enabled"`
	// Version is a metrics-server release tag (e.g. "v0.7.2"); empty = latest.
	Version string `yaml:"version"`
	// KubeletInsecureTLS adds --kubelet-insecure-tls (required on kind).
	// nil = true.
	KubeletInsecureTLS *bool `yaml:"kubeletInsecureTLS"`
}

// Parse decodes a ClusterSet manifest from YAML bytes (strict: unknown fields error).
func Parse(data []byte) (*ClusterSet, error) {
	var cs ClusterSet
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cs); err != nil {
		return nil, fmt.Errorf("parsing ClusterSet manifest: %w", err)
	}
	return &cs, nil
}

// Validate checks the manifest for structural and referential errors.
func (cs *ClusterSet) Validate() error {
	if cs.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", cs.APIVersion, APIVersion)
	}
	if cs.Kind != Kind {
		return fmt.Errorf("unsupported kind %q (want %q)", cs.Kind, Kind)
	}
	switch cs.Spec.Strategy {
	case "", StrategyFailFast, StrategyContinueOnError:
	default:
		return fmt.Errorf("invalid strategy %q (want %q or %q)", cs.Spec.Strategy, StrategyFailFast, StrategyContinueOnError)
	}
	if cs.Spec.MaxParallel < 0 {
		return fmt.Errorf("maxParallel must be >= 0, got %d", cs.Spec.MaxParallel)
	}
	if len(cs.Spec.Clusters) == 0 {
		return fmt.Errorf("spec.clusters must list at least one cluster")
	}

	names := make(map[string]bool, len(cs.Spec.Clusters))
	explicitNums := make(map[int]string)
	for i, c := range cs.Spec.Clusters {
		if c.Name == "" {
			return fmt.Errorf("spec.clusters[%d]: name is required", i)
		}
		if !clusterNameRE.MatchString(c.Name) {
			return fmt.Errorf("invalid cluster name %q (lowercase alphanumeric, '-' and '.' only)", c.Name)
		}
		if names[c.Name] {
			return fmt.Errorf("duplicate cluster name %q", c.Name)
		}
		names[c.Name] = true

		if c.Num < 0 || c.Num > 99 {
			return fmt.Errorf("cluster %q: num must be 1-99 (or 0 to auto-assign), got %d", c.Name, c.Num)
		}
		if c.Num != 0 {
			if other, dup := explicitNums[c.Num]; dup {
				return fmt.Errorf("clusters %q and %q both request num %d", other, c.Name, c.Num)
			}
			explicitNums[c.Num] = c.Name
		}
	}

	// dependsOn references must exist, must not self-reference.
	for _, c := range cs.Spec.Clusters {
		for _, d := range c.DependsOn {
			if d == c.Name {
				return fmt.Errorf("cluster %q depends on itself", c.Name)
			}
			if !names[d] {
				return fmt.Errorf("cluster %q dependsOn unknown cluster %q", c.Name, d)
			}
		}
	}

	if cycle := cs.detectCycle(); cycle != "" {
		return fmt.Errorf("dependency cycle detected: %s", cycle)
	}
	return nil
}

// detectCycle returns a human-readable cycle path, or "" if the DAG is acyclic.
func (cs *ClusterSet) detectCycle() string {
	deps := make(map[string][]string, len(cs.Spec.Clusters))
	for _, c := range cs.Spec.Clusters {
		deps[c.Name] = c.DependsOn
	}
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := make(map[string]int, len(deps))
	var stack []string
	var visit func(n string) []string
	visit = func(n string) []string {
		color[n] = gray
		stack = append(stack, n)
		for _, d := range deps[n] {
			switch color[d] {
			case gray:
				return append(stack, d) // found a back-edge
			case white:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		color[n] = black
		stack = stack[:len(stack)-1]
		return nil
	}
	for _, c := range cs.Spec.Clusters {
		if color[c.Name] == white {
			if cyc := visit(c.Name); cyc != nil {
				return strings.Join(cyc, " → ")
			}
		}
	}
	return ""
}

// Merged returns a copy of the entry with unset fields filled from defaults.
func (c ClusterEntry) Merged(d Defaults) ClusterEntry {
	if c.NodeVersion == "" {
		c.NodeVersion = d.NodeVersion
	}
	if c.Region == "" {
		c.Region = d.Region
	}
	if c.Zone == "" {
		c.Zone = d.Zone
	}
	if c.Registries == nil {
		c.Registries = d.Registries
	}
	if c.Addons == nil {
		c.Addons = d.Addons
	}
	return c
}
