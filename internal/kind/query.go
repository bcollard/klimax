package kind

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/bcollard/klimax/internal/guest"
)

// selectorRE guards against shell-metacharacter injection when embedding a label
// selector into an in-guest kubectl command. Real label selectors only use these
// characters (keys, values, operators, commas, parentheses, spaces).
var selectorRE = regexp.MustCompile(`^[A-Za-z0-9_.\-/=!(),\s]*$`)

func validateSelector(sel string) error {
	if !selectorRE.MatchString(sel) {
		return fmt.Errorf("invalid label selector %q", sel)
	}
	return nil
}

// ClustersMatchingSelector returns the names of clusters whose nodes match the
// given kubectl label selector (e.g. "klimax.dev/fleet=f1,env=prod"). Selector
// parsing is delegated to kubectl in the guest.
func ClustersMatchingSelector(ctx context.Context, g *guest.Client, selector string) ([]string, error) {
	if err := validateSelector(selector); err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf(`for c in $(kind get clusters 2>/dev/null); do
  kc=/tmp/klimax-kube-$c.yaml
  kind get kubeconfig --name "$c" | sed 's|https://0.0.0.0:|https://127.0.0.1:|g' > "$kc" 2>/dev/null
  if [ -n "$(kubectl --kubeconfig "$kc" get nodes -l '%s' -o name 2>/dev/null)" ]; then echo "$c"; fi
done`, selector)
	out, err := g.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("selecting clusters by label: %w", err)
	}
	return nonEmptyLines(out), nil
}

// ClustersByFleet groups every cluster by its klimax.dev/fleet node label value.
// Clusters without the label are grouped under the empty string.
func ClustersByFleet(ctx context.Context, g *guest.Client) (map[string][]string, error) {
	cmd := `for c in $(kind get clusters 2>/dev/null); do
  kc=/tmp/klimax-kube-$c.yaml
  kind get kubeconfig --name "$c" | sed 's|https://0.0.0.0:|https://127.0.0.1:|g' > "$kc" 2>/dev/null
  f=$(kubectl --kubeconfig "$kc" get nodes -o json 2>/dev/null | jq -r '.items[0].metadata.labels["klimax.dev/fleet"] // ""')
  echo "$c|$f"
done`
	out, err := g.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("grouping clusters by fleet: %w", err)
	}
	byFleet := map[string][]string{}
	for _, line := range nonEmptyLines(out) {
		name, fleet, _ := strings.Cut(line, "|")
		byFleet[fleet] = append(byFleet[fleet], name)
	}
	return byFleet, nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			out = append(out, l)
		}
	}
	return out
}
