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
# Docker names the bridge br-<first 12 chars of the network id>. An earlier
# version of this script grepped the id out of "ip link" without the br- prefix,
# which yielded a name no interface has: iptables accepts any -o name, so Rule 3
# silently never matched (Rule 2 carried the traffic). Look the name up exactly.
KIND_IF=""
if ip link show "br-${KIND_NET_ID}" >/dev/null 2>&1; then
  KIND_IF="br-${KIND_NET_ID}"
fi
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
# Drop the prefix-less rule the old interface lookup installed.
iptables -D DOCKER-USER -s "${HOST_GW}/32" -d "${KIND_CIDR}" \
  -i "${HOST_IF}" -o "${KIND_NET_ID}" -j ACCEPT 2>/dev/null || true
iptables -C DOCKER-USER -s "${HOST_GW}/32" -d "${KIND_CIDR}" \
  -i "${HOST_IF}" -o "${KIND_IF}" -j ACCEPT 2>/dev/null \
  || iptables -I DOCKER-USER 2 -s "${HOST_GW}/32" -d "${KIND_CIDR}" \
       -i "${HOST_IF}" -o "${KIND_IF}" -j ACCEPT

# Local DNS (network.dns). Empty DNS_IP means the feature is off, and anything
# a previous run installed is removed.
DNS_IP="{{ .DNSIP }}"
DNS_DOMAIN="{{ .DNSDomain }}"

# Rule 4: Docker drops traffic to a container address that does not arrive on
# its bridge (a per-container raw PREROUTING DROP, which runs before DOCKER-USER
# is ever consulted). That blocks the host from the DNS container, so exempt
# exactly that one address, from the host gateway only. Docker appends its own
# rules, so an insert at the top stays ahead of them. Tagged with a comment so
# it can be found and removed without knowing the address it was written for.
# grep exits 1 on no match, which pipefail + set -e would turn into a failed run.
DNS_RULES=$(iptables -t raw -S PREROUTING | grep -- '--comment klimax-dns' || true)
while read -r RULE; do
  [ -z "${RULE}" ] && continue
  if [ -n "${DNS_IP}" ] && [[ "${RULE}" == *"-d ${DNS_IP}/32"* ]] && [[ "${RULE}" == *"-s ${HOST_GW}/32"* ]]; then
    continue
  fi
  eval "iptables -t raw ${RULE/-A PREROUTING/-D PREROUTING}"
done <<< "${DNS_RULES}"
if [ -n "${DNS_IP}" ]; then
  iptables -t raw -C PREROUTING -i "${HOST_IF}" -s "${HOST_GW}/32" -d "${DNS_IP}/32" \
    -m comment --comment klimax-dns -j ACCEPT 2>/dev/null \
    || iptables -t raw -I PREROUTING 1 -i "${HOST_IF}" -s "${HOST_GW}/32" -d "${DNS_IP}/32" \
         -m comment --comment klimax-dns -j ACCEPT

  # Route the zone for the VM itself (dockerd pulls, klimax shell). A
  # route-only domain on the kind bridge link: only names under it go to the
  # DNS container, and the bridge never becomes a default DNS route. Re-applied
  # here because Docker recreates the bridge link on restart.
  if command -v resolvectl >/dev/null 2>&1; then
    resolvectl dns "${KIND_IF}" "${DNS_IP}" || true
    resolvectl domain "${KIND_IF}" "~${DNS_DOMAIN}" || true
    resolvectl default-route "${KIND_IF}" false || true
  fi
elif command -v resolvectl >/dev/null 2>&1; then
  resolvectl revert "${KIND_IF}" 2>/dev/null || true
fi

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

// LocalDNS is the part of network.dns the routing rules need. The zero value
// means the feature is off.
type LocalDNS struct {
	ServerIP string
	Domain   string
}

// InstallNoNat installs the no-NAT routing rules in the guest VM and
// sets up systemd persistence. Idempotent.
func InstallNoNat(ctx context.Context, g *guest.Client, kindCIDR string, dns LocalDNS) error {
	slog.Info("Installing no-NAT routing rules in guest", "kindCIDR", kindCIDR)

	script := renderNoNatScript(kindCIDR, dns)

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

// renderNoNatScript fills the script's placeholders.
func renderNoNatScript(kindCIDR string, dns LocalDNS) string {
	return strings.NewReplacer(
		`{{ .KindCIDR }}`, kindCIDR,
		`{{ .DNSIP }}`, dns.ServerIP,
		`{{ .DNSDomain }}`, dns.Domain,
	).Replace(noNatScript)
}

// CheckDNSRule reports whether the raw-table exemption for the DNS container is
// present — without it the Mac's queries are dropped by Docker before they reach
// the container.
func CheckDNSRule(ctx context.Context, g *guest.Client, serverIP string) (bool, error) {
	out, err := g.Run(ctx, fmt.Sprintf(
		`sudo iptables -t raw -S PREROUTING | grep -- '--comment klimax-dns' | grep -q -- '-d %s/32' && echo yes || echo no`, serverIP))
	if err != nil {
		return false, fmt.Errorf("checking DNS iptables rule: %w", err)
	}
	return strings.TrimSpace(out) == "yes", nil
}

// CheckNoNatRule returns true if the NAT exemption rule is present in the guest.
//
// The probe must exactly reconstruct the rule installed by noNatScript (Rule 1),
// including the `-d "${VM_NET}"` destination qualifier — `iptables -C` requires an
// exact spec match, so a `-d`-less probe never matches the installed `-d` rule and
// would always report the rule as missing. VM_NET is derived from lima0's IP as a
// /24 via python3's ipaddress module, identically to noNatScript.
//
// The probe also runs under sudo: the guest SSH user is `lima` (unprivileged) and
// iptables requires root, so an unsudoed probe fails with a permission error that
// `2>/dev/null` swallows into a false "missing" result.
func CheckNoNatRule(ctx context.Context, g *guest.Client, kindCIDR string) (bool, error) {
	cmd := fmt.Sprintf(
		`HOST_IF=lima0
VM_IP=$(ip -o -4 addr show ${HOST_IF} | awk '{print $4}' | cut -d/ -f1)
VM_NET=$(python3 -c "import ipaddress; print(ipaddress.ip_interface('${VM_IP}/24').network)")
sudo iptables -t nat -C POSTROUTING -s "%s" -d "${VM_NET}" -o "${HOST_IF}" -j ACCEPT 2>/dev/null && echo yes || echo no`,
		kindCIDR,
	)
	out, err := g.Run(ctx, cmd)
	if err != nil {
		return false, fmt.Errorf("checking iptables rule: %w", err)
	}
	return strings.TrimSpace(out) == "yes", nil
}
