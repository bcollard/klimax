package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Kubernetes label syntax (approximate): an optional DNS-subdomain prefix and a
// name segment of alphanumerics plus -_. (≤63 chars); values follow the same
// name rules and may be empty.
var (
	labelKeyRE   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	labelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
)

// ParseLabels turns "key=value" strings (e.g. from a repeatable CLI flag) into a
// validated label map. Returns nil for an empty input.
func ParseLabels(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("invalid label %q: expected key=value", p)
		}
		m[k] = v
	}
	if err := ValidateLabels(m); err != nil {
		return nil, err
	}
	return m, nil
}

// ValidateLabels checks that every key/value is a syntactically valid Kubernetes label.
func ValidateLabels(m map[string]string) error {
	for k, v := range m {
		if len(k) > 253 || !labelKeyRE.MatchString(k) {
			return fmt.Errorf("invalid label key %q", k)
		}
		if len(v) > 63 || !labelValueRE.MatchString(v) {
			return fmt.Errorf("invalid label value %q (key %q)", v, k)
		}
	}
	return nil
}
