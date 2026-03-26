package routing

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bcollard/klimax/internal/guest"
)

// noNatScript is installed at /usr/local/sbin/no-nat-kind.sh in the guest.
// It detects the host gateway IP and kind bridge interface dynamically, then
// applies three iptables rules idempotently to enable pure L3 routing without SNAT.
const noNatScript = `#!/bin/bash
set -euo pipefail

KIND_CIDR="{{ .KindCIDR }}"
HOST_IF=lima0

# Detect VM's lima0 IP (e.g. 192.168.105.2)
VM_IP=$(ip -o -4 addr show ${HOST_IF} | awk '{print $4}' | cut -d/ -f1)
# Derive /24 network (e.g. 192.168.105.0/24)
VM_NET=$(python3 -c "import ipaddress; print(ipaddress.ip_interface('${VM_IP}/24').network)")
# Host gateway is first IP in that /24 (e.g. 192.168.105.1)
HOST_GW=$(python3 -c "import ipaddress; print(list(ipaddress.ip_network('${VM_NET}').hosts())[0])")

# Detect kind bridge interface (br-<id>) from "kind" docker network
KIND_NET_ID=$(docker network inspect kind --format '{{.Id}}' 2>/dev/null | cut -c1-12 || true)
if [ -z "${KIND_NET_ID}" ]; then
  echo "WARNING: Docker network 'kind' not found, skipping iptables setup"
  exit 0
fi
KIND_IF=$(ip link show | grep -o "${KIND_NET_ID}[^:]*" | head -n1 | tr -d ' ' || true)
if [ -z "${KIND_IF}" ]; then
  echo "WARNING: Could not find bridge interface for kind network ${KIND_NET_ID}"
  exit 0
fi

echo "Applying no-NAT rules: KIND_CIDR=${KIND_CIDR} VM_IP=${VM_IP} HOST_GW=${HOST_GW} KIND_IF=${KIND_IF}"

# Rule 1: NAT exemption — replies from kind subnet to host subnet bypass MASQUERADE
iptables -t nat -C POSTROUTING -s "${KIND_CIDR}" -d "${VM_NET}" -o "${HOST_IF}" -j ACCEPT 2>/dev/null \
  || iptables -t nat -I POSTROUTING 1 -s "${KIND_CIDR}" -d "${VM_NET}" -o "${HOST_IF}" -j ACCEPT

# Rule 2: Allow forwarding from host gateway to kind subnet (new/established)
iptables -C DOCKER-USER -s "${HOST_GW}/32" -d "${KIND_CIDR}" \
  -m conntrack --ctstate NEW,RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
  || iptables -I DOCKER-USER 1 -s "${HOST_GW}/32" -d "${KIND_CIDR}" \
       -m conntrack --ctstate NEW,RELATED,ESTABLISHED -j ACCEPT

# Rule 3: Allow forwarding from lima0 to kind bridge
iptables -C DOCKER-USER -s "${HOST_GW}/32" -d "${KIND_CIDR}" \
  -i "${HOST_IF}" -o "${KIND_IF}" -j ACCEPT 2>/dev/null \
  || iptables -I DOCKER-USER 2 -s "${HOST_GW}/32" -d "${KIND_CIDR}" \
       -i "${HOST_IF}" -o "${KIND_IF}" -j ACCEPT

echo "no-nat-kind rules applied successfully"
`

// noNatServiceUnit is the systemd service that runs noNatScript at boot.
const noNatServiceUnit = `[Unit]
Description=Klimax no-NAT kind routing rules
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/no-nat-kind.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`

// dockerDropIn hooks noNatScript to re-run every time Docker restarts
// (Docker rewrites iptables on restart, wiping our rules).
const dockerDropIn = `[Service]
ExecStartPost=/usr/local/sbin/no-nat-kind.sh
`

// InstallNoNat installs the no-NAT routing rules in the guest VM and
// sets up systemd persistence. Idempotent.
func InstallNoNat(ctx context.Context, g *guest.Client, kindCIDR string) error {
	slog.Info("Installing no-NAT routing rules in guest", "kindCIDR", kindCIDR)

	// Substitute the kindCIDR placeholder.
	script := strings.ReplaceAll(noNatScript, `{{ .KindCIDR }}`, kindCIDR)

	installScript := fmt.Sprintf(`#!/bin/bash
set -euo pipefail

# Install the no-nat script
cat > /usr/local/sbin/no-nat-kind.sh <<'SCRIPT_EOF'
%s
SCRIPT_EOF
chmod +x /usr/local/sbin/no-nat-kind.sh

# Install systemd service unit
cat > /etc/systemd/system/lima-no-nat-kind.service <<'UNIT_EOF'
%s
UNIT_EOF

# Install docker drop-in to reapply rules after Docker restarts
mkdir -p /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/99-no-nat-kind.conf <<'DROPIN_EOF'
%s
DROPIN_EOF

systemctl daemon-reload
systemctl enable --now lima-no-nat-kind.service
`, script, noNatServiceUnit, dockerDropIn)

	if err := g.RunScript(ctx, "install no-nat rules", installScript); err != nil {
		return fmt.Errorf("installing no-NAT rules: %w", err)
	}

	// Apply rules immediately.
	slog.Info("Applying no-NAT rules immediately")
	if _, err := g.Run(ctx, "sudo /usr/local/sbin/no-nat-kind.sh"); err != nil {
		return fmt.Errorf("applying no-NAT rules: %w", err)
	}
	return nil
}

// CheckNoNatRule returns true if the NAT exemption rule is present in the guest.
func CheckNoNatRule(ctx context.Context, g *guest.Client, kindCIDR string) (bool, error) {
	cmd := fmt.Sprintf(
		`iptables -t nat -C POSTROUTING -s "%s" -o lima0 -j ACCEPT 2>/dev/null && echo yes || echo no`,
		kindCIDR,
	)
	out, err := g.Run(ctx, cmd)
	if err != nil {
		return false, fmt.Errorf("checking iptables rule: %w", err)
	}
	return strings.TrimSpace(out) == "yes", nil
}
