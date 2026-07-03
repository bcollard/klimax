package clusterset

import "fmt"

// PlannedCluster is a cluster entry with defaults merged, its num resolved, and
// a flag for whether it already exists in the VM.
type PlannedCluster struct {
	ClusterEntry
	Num    int
	Exists bool
}

// Plan is the resolved, ready-to-execute view of a ClusterSet.
type Plan struct {
	Name        string
	MaxParallel int
	Strategy    string
	Clusters    []PlannedCluster // manifest order
}

// ToCreate returns the planned clusters that do not yet exist.
func (p *Plan) ToCreate() []PlannedCluster {
	var out []PlannedCluster
	for _, c := range p.Clusters {
		if !c.Exists {
			out = append(out, c)
		}
	}
	return out
}

// Resolve merges defaults, marks which clusters already exist, and pre-assigns
// nums for the clusters to create. Pre-assigning up front (rather than letting
// each create race for the next free slot) is what makes parallel creation safe.
//
// existing maps live cluster num → name (from kind.DetectUsedNums).
func (cs *ClusterSet) Resolve(existing map[int]string) (*Plan, error) {
	existingByName := make(map[string]int, len(existing))
	for num, name := range existing {
		existingByName[name] = num
	}

	plan := &Plan{
		Name:        cs.Metadata.Name,
		MaxParallel: cs.Spec.MaxParallel,
		Strategy:    cs.Spec.Strategy,
	}
	if plan.MaxParallel < 1 {
		plan.MaxParallel = 1
	}
	if plan.Strategy == "" {
		plan.Strategy = StrategyFailFast
	}

	// reserved = nums unavailable for auto-assignment: live clusters + explicit
	// manifest nums.
	reserved := make(map[int]bool, len(existing))
	for n := range existing {
		reserved[n] = true
	}
	for _, c := range cs.Spec.Clusters {
		if c.Num != 0 {
			if owner, taken := existing[c.Num]; taken && owner != c.Name {
				return nil, fmt.Errorf("cluster %q requests num %d, already used by live cluster %q", c.Name, c.Num, owner)
			}
			reserved[c.Num] = true
		}
	}

	next := 1
	freeNum := func() (int, error) {
		for next <= 99 {
			n := next
			next++
			if !reserved[n] {
				reserved[n] = true
				return n, nil
			}
		}
		return 0, fmt.Errorf("no free cluster num available (1-99 exhausted)")
	}

	for _, c := range cs.Spec.Clusters {
		pc := PlannedCluster{ClusterEntry: c.Merged(cs.Spec.Defaults)}
		switch num, ok := existingByName[c.Name]; {
		case ok:
			pc.Exists = true
			pc.Num = num
		case c.Num != 0:
			pc.Num = c.Num
		default:
			n, err := freeNum()
			if err != nil {
				return nil, err
			}
			pc.Num = n
		}
		plan.Clusters = append(plan.Clusters, pc)
	}
	return plan, nil
}
