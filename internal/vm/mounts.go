package vm

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"
	"gopkg.in/yaml.v3"
)

// limaYAMLIndent is the indentation of a Lima instance config. Both Lima and
// klimax write these files with gopkg.in/yaml.v3 at its default indent, so
// re-encoding at anything else would reflow the whole document for no reason.
const limaYAMLIndent = 4

// InstanceYAMLPath is the Lima instance config inside an instance directory.
func InstanceYAMLPath(instDir string) string {
	return filepath.Join(instDir, filenames.LimaYAML)
}

// NormalizedMount is a mount reduced to the three fields klimax controls, with
// Lima's defaults filled in, so two mount sets can be compared directly.
type NormalizedMount struct {
	// HostPath is the shared directory on macOS.
	HostPath string
	// GuestPath is where it appears in the VM — the host path unless an
	// explicit mountPoint says otherwise.
	GuestPath string
	// Writable is Lima's `writable`, which defaults to false.
	Writable bool
}

// String renders a mount the way the CLI reports it.
func (n NormalizedMount) String() string {
	mode := "ro"
	if n.Writable {
		mode = "rw"
	}
	if n.GuestPath == n.HostPath {
		return fmt.Sprintf("%s (%s)", n.HostPath, mode)
	}
	return fmt.Sprintf("%s → %s (%s)", n.HostPath, n.GuestPath, mode)
}

// NormalizeMounts applies Lima's defaulting rules (mountPoint defaults to
// location, writable defaults to false) so mount sets from different sources —
// a freshly built config and a live instance file — can be compared.
func NormalizeMounts(mounts []limatype.Mount) []NormalizedMount {
	out := make([]NormalizedMount, 0, len(mounts))
	for _, m := range mounts {
		n := NormalizedMount{
			HostPath:  filepath.Clean(m.Location),
			GuestPath: filepath.Clean(m.Location),
		}
		if m.MountPoint != nil && *m.MountPoint != "" {
			n.GuestPath = filepath.Clean(*m.MountPoint)
		}
		if m.Writable != nil {
			n.Writable = *m.Writable
		}
		out = append(out, n)
	}
	return out
}

// MountsEqual reports whether two mount sets describe the same shares.
func MountsEqual(a, b []limatype.Mount) bool {
	return slices.Equal(NormalizeMounts(a), NormalizeMounts(b))
}

// DescribeMounts renders a mount set as one indented line per entry, for CLI
// output. An empty set renders as "(none)".
func DescribeMounts(mounts []limatype.Mount) string {
	norm := NormalizeMounts(mounts)
	if len(norm) == 0 {
		return "    (none)"
	}
	lines := make([]string, 0, len(norm))
	for _, n := range norm {
		lines = append(lines, "    "+n.String())
	}
	return strings.Join(lines, "\n")
}

// ReadInstanceMounts returns the mounts declared in a Lima instance config.
//
// It reads the on-disk file rather than limatype.Instance.Config so the result
// is what Lima will actually read on the next start, with no defaults filled in
// by an inspect.
func ReadInstanceMounts(path string) ([]limatype.Mount, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var y struct {
		Mounts []limatype.Mount `yaml:"mounts"`
	}
	if err := yaml.Unmarshal(data, &y); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return y.Mounts, nil
}

// WriteInstanceMounts replaces the `mounts:` block of a Lima instance config,
// leaving the rest of the document alone.
//
// The edit is done on the parsed node tree rather than by regenerating the file
// from limatemplate.Build: the instance config carries provisioning scripts that
// the guest has already run, and rewriting those on an existing VM would mean a
// klimax upgrade silently changed how a live guest is provisioned. Node-level
// surgery keeps the blast radius to the one key.
//
// The VM must be stopped: Lima reads this file at start, so an edit under a
// running instance is at best ignored and at worst confuses a later restart.
func WriteInstanceMounts(path string, mounts []limatype.Mount) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a YAML mapping document", path)
	}
	root := doc.Content[0]

	idx := mappingKeyIndex(root, "mounts")
	switch {
	case len(mounts) == 0:
		if idx >= 0 {
			// Drop the key entirely rather than writing `mounts: []`: Lima's
			// own schema marks mounts omitempty, so absent is its idle state.
			root.Content = slices.Delete(root.Content, idx, idx+2)
		}
	default:
		var val yaml.Node
		if err := val.Encode(mounts); err != nil {
			return fmt.Errorf("encoding mounts: %w", err)
		}
		if idx >= 0 {
			root.Content[idx+1] = &val
		} else {
			key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mounts"}
			// Sit next to mountType when there is one, so the file keeps the
			// field grouping limatemplate.Build produces.
			at := mappingKeyIndex(root, "mountType")
			if at < 0 {
				at = len(root.Content)
			}
			root.Content = slices.Insert(root.Content, at, key, &val)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(limaYAMLIndent)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	return os.WriteFile(path, buf.Bytes(), fi.Mode().Perm())
}

// mappingKeyIndex returns the index of key's *key node* in a mapping node's
// flat key/value Content slice, or -1. The value sits at the following index.
func mappingKeyIndex(mapping *yaml.Node, key string) int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return i
		}
	}
	return -1
}
