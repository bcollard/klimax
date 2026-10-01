package vm

import (
	"fmt"
	"os"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"gopkg.in/yaml.v3"
)

// ReadInstancePortForwards returns the portForwards declared in a Lima instance
// config, read from disk for the same reason as ReadInstanceMounts.
func ReadInstancePortForwards(path string) ([]limatype.PortForward, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var y struct {
		PortForwards []limatype.PortForward `yaml:"portForwards"`
	}
	if err := yaml.Unmarshal(data, &y); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return y.PortForwards, nil
}

// WriteInstancePortForwards replaces the `portForwards:` block of a Lima
// instance config, leaving the rest of the document alone. The VM must be
// stopped, as for WriteInstanceMounts.
func WriteInstancePortForwards(path string, fwds []limatype.PortForward) error {
	return writeInstanceKey(path, "portForwards", fwds, len(fwds) == 0, "")
}

// PortMirroringDisabled reports whether a portForwards list carries the
// catch-all TCP ignore rule that turns off Lima's automatic mirroring of guest
// ports to 127.0.0.1 — the rule limatemplate emits for
// network.disablePortMirroring: true.
func PortMirroringDisabled(fwds []limatype.PortForward) bool {
	for _, f := range fwds {
		if f.Ignore && f.Proto == limatype.ProtoTCP &&
			f.GuestPortRange == [2]int{1, 65535} &&
			f.GuestIP != nil && f.GuestIP.IsUnspecified() {
			return true
		}
	}
	return false
}
