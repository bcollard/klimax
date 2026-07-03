package fleet

import "testing"

// minimal manifest: only cluster names, everything else defaulted.
func TestParseMinimalNamesOnly(t *testing.T) {
	data := []byte(`apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  clusters:
    - dev
    - staging
`)
	cs, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cs.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(cs.Spec.Clusters) != 2 {
		t.Fatalf("want 2 clusters, got %d", len(cs.Spec.Clusters))
	}
	if cs.Spec.Clusters[0].Name != "dev" || cs.Spec.Clusters[1].Name != "staging" {
		t.Fatalf("unexpected names: %+v", cs.Spec.Clusters)
	}
}

// entries may mix bare strings and full objects.
func TestParseMixedEntries(t *testing.T) {
	data := []byte(`apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  clusters:
    - hub
    - name: spoke
      dependsOn: [hub]
      num: 5
      registries:
        mirrors: ["registry-dockerio"]
      addons:
        metricsServer:
          enabled: true
`)
	cs, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cs.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	spoke := cs.Spec.Clusters[1]
	if spoke.Name != "spoke" || len(spoke.DependsOn) != 1 || spoke.Num != 5 {
		t.Fatalf("unexpected spoke: %+v", spoke)
	}
	if spoke.Registries == nil || spoke.Registries.Mirrors == nil || (*spoke.Registries.Mirrors)[0] != "registry-dockerio" {
		t.Fatalf("unexpected registries: %+v", spoke.Registries)
	}
	if spoke.Addons == nil || spoke.Addons.MetricsServer == nil || !spoke.Addons.MetricsServer.Enabled {
		t.Fatalf("unexpected addons: %+v", spoke.Addons)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"bad apiVersion": `apiVersion: v1
kind: Fleet
spec: {clusters: [a]}`,
		"bad kind": `apiVersion: klimax.dev/v1alpha1
kind: Pod
spec: {clusters: [a]}`,
		"no clusters": `apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec: {clusters: []}`,
		"dup name": `apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec: {clusters: [a, a]}`,
		"unknown dep": `apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  clusters:
    - name: a
      dependsOn: [ghost]`,
		"cycle": `apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  clusters:
    - name: a
      dependsOn: [b]
    - name: b
      dependsOn: [a]`,
		"dup num": `apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  clusters:
    - name: a
      num: 3
    - name: b
      num: 3`,
	}
	for name, y := range cases {
		cs, err := Parse([]byte(y))
		if err == nil {
			err = cs.Validate()
		}
		if err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestResolveNumAssignment(t *testing.T) {
	data := []byte(`apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  clusters:
    - name: keep       # already exists at num 2
    - name: pinned
      num: 5
    - name: auto1
    - name: auto2
`)
	cs, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cs.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	existing := map[int]string{2: "keep"} // live cluster "keep" at num 2
	plan, err := cs.Resolve(existing)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	byName := map[string]PlannedCluster{}
	for _, pc := range plan.Clusters {
		byName[pc.Name] = pc
	}
	if !byName["keep"].Exists || byName["keep"].Num != 2 {
		t.Errorf("keep: want existing num 2, got %+v", byName["keep"])
	}
	if byName["pinned"].Num != 5 || byName["pinned"].Exists {
		t.Errorf("pinned: want new num 5, got %+v", byName["pinned"])
	}
	// auto1/auto2 must avoid reserved nums {2 (live), 5 (pinned)} → lowest free is 1, then 3.
	if byName["auto1"].Num != 1 {
		t.Errorf("auto1: want num 1, got %d", byName["auto1"].Num)
	}
	if byName["auto2"].Num != 3 {
		t.Errorf("auto2: want num 3, got %d", byName["auto2"].Num)
	}
	if n := len(plan.ToCreate()); n != 3 {
		t.Errorf("want 3 clusters to create, got %d", n)
	}
	if plan.MaxParallel != 1 || plan.Strategy != StrategyFailFast {
		t.Errorf("defaults: maxParallel=%d strategy=%s", plan.MaxParallel, plan.Strategy)
	}
}

func TestLabelsMergeWithDefaults(t *testing.T) {
	data := []byte(`apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  defaults:
    labels:
      tier: shared
      env: dev
  clusters:
    - name: a
      labels:
        env: prod
        team: platform
`)
	cs, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := cs.Spec.Clusters[0].Merged(cs.Spec.Defaults).Labels
	want := map[string]string{"tier": "shared", "env": "prod", "team": "platform"}
	if len(m) != len(want) {
		t.Fatalf("merged labels = %v, want %v", m, want)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("label %q = %q, want %q", k, m[k], v)
		}
	}
}

func TestResolvePinnedNumConflictsWithLive(t *testing.T) {
	data := []byte(`apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  clusters:
    - name: newone
      num: 2
`)
	cs, _ := Parse(data)
	if err := cs.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := cs.Resolve(map[int]string{2: "other"}); err == nil {
		t.Fatal("expected conflict error for num 2 already used by live cluster")
	}
}
