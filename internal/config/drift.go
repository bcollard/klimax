package config

import (
	"sort"

	"gopkg.in/yaml.v3"
)

// MissingKeys returns dotted key paths that the current default config schema
// defines but the user's raw config omits. It's a best-effort signal that a
// config file predates options added in a newer klimax version (defaults apply
// for the missing keys). A freshly generated config yields an empty list.
func MissingKeys(rawUser []byte) ([]string, error) {
	def := &Config{}
	applyDefaults(def)
	defBytes, err := yaml.Marshal(def)
	if err != nil {
		return nil, err
	}

	var defMap, userMap map[string]any
	if err := yaml.Unmarshal(defBytes, &defMap); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(rawUser, &userMap); err != nil {
		return nil, err
	}

	var missing []string
	diffMissingKeys("", defMap, userMap, &missing)
	sort.Strings(missing)
	return missing, nil
}

// diffMissingKeys walks the default map and records any key path absent from the
// user map. Recurses into nested mappings; lists and scalars are compared only
// for presence (no deep list diffing, to avoid noise).
func diffMissingKeys(prefix string, def, user map[string]any, out *[]string) {
	for k, dv := range def {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		uv, present := user[k]
		if !present {
			*out = append(*out, path)
			continue
		}
		if dm, ok := dv.(map[string]any); ok {
			if um, ok := uv.(map[string]any); ok {
				diffMissingKeys(path, dm, um, out)
			}
		}
	}
}
