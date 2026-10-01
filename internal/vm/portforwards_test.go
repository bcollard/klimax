package vm

import (
	"net"
	"strings"
	"testing"

	"github.com/lima-vm/lima/v2/pkg/limatype"
)

// portForwardsBlock is the portForwards section of a VM created with
// network.disablePortMirroring: true, as marina writes it.
const portForwardsBlock = `portForwards:
    - guestIP: 0.0.0.0
      guestPortRange:
        - 1
        - 65535
      hostPortRange:
        - 0
        - 0
      proto: tcp
      ignore: true
    - guestPortRange:
        - 0
        - 0
      guestSocket: /run/docker.sock
      hostPortRange:
        - 0
        - 0
      hostSocket: '{{.Home}}/.marina.docker.sock'
`

var dockerSocketForward = limatype.PortForward{
	GuestSocket: "/run/docker.sock",
	HostSocket:  "{{.Home}}/.marina.docker.sock",
}

var ignoreAllTCP = limatype.PortForward{
	GuestIP:        net.IPv4zero,
	GuestPortRange: [2]int{1, 65535},
	Proto:          limatype.ProtoTCP,
	Ignore:         true,
}

func TestReadInstancePortForwardsDirectMode(t *testing.T) {
	got, err := ReadInstancePortForwards(writeFixture(t, limaYAMLFixture+portForwardsBlock))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d forwards, want 2", len(got))
	}
	if !PortMirroringDisabled(got) {
		t.Error("catch-all ignore rule not detected")
	}
}

func TestPortMirroringDisabled(t *testing.T) {
	cases := []struct {
		name string
		fwds []limatype.PortForward
		want bool
	}{
		{"none", nil, false},
		{"socket only", []limatype.PortForward{dockerSocketForward}, false},
		{"catch-all", []limatype.PortForward{ignoreAllTCP, dockerSocketForward}, true},
		{"partial range", []limatype.PortForward{{GuestIP: net.IPv4zero, GuestPortRange: [2]int{7000, 7099}, Proto: limatype.ProtoTCP, Ignore: true}}, false},
		{"loopback only", []limatype.PortForward{{GuestIP: net.IPv4(127, 0, 0, 1), GuestPortRange: [2]int{1, 65535}, Proto: limatype.ProtoTCP, Ignore: true}}, false},
	}
	for _, c := range cases {
		if got := PortMirroringDisabled(c.fwds); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Switching either way must round-trip through YAML (net.IP included) and leave
// the provisioning scripts byte for byte.
func TestWriteInstancePortForwardsToggles(t *testing.T) {
	path := writeFixture(t, limaYAMLFixture+portForwardsBlock)

	if err := WriteInstancePortForwards(path, []limatype.PortForward{dockerSocketForward}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadInstancePortForwards(path)
	if err != nil {
		t.Fatal(err)
	}
	if PortMirroringDisabled(got) || len(got) != 1 || got[0].GuestSocket != "/run/docker.sock" {
		t.Fatalf("after enabling mirroring: %+v", got)
	}
	if !strings.Contains(readFile(t, path), "# Find the disk by label, not by device order.") {
		t.Error("provision script was not preserved")
	}

	if err := WriteInstancePortForwards(path, []limatype.PortForward{ignoreAllTCP, dockerSocketForward}); err != nil {
		t.Fatal(err)
	}
	got, err = ReadInstancePortForwards(path)
	if err != nil {
		t.Fatal(err)
	}
	if !PortMirroringDisabled(got) {
		t.Fatalf("after disabling mirroring: %+v", got)
	}
}
