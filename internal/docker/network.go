package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/bcollard/klimax/internal/guest"
)

// EnsureKindNetwork ensures a Docker bridge network named "kind" exists in the
// guest with the requested CIDR subnet.
//
//   - If it already exists with the correct subnet → no-op.
//   - If it exists with a different subnet → returns an error (manual fix required).
//   - If it doesn't exist → creates it.
func EnsureKindNetwork(ctx context.Context, g *guest.Client, cidr string) error {
	existing, err := inspectKindSubnet(ctx, g)
	if err != nil {
		return err
	}

	if existing != "" {
		// Normalize both CIDRs for comparison.
		_, wantNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid kindBridgeCIDR %q: %w", cidr, err)
		}
		_, gotNet, err := net.ParseCIDR(existing)
		if err != nil {
			return fmt.Errorf("parsing existing kind network CIDR %q: %w", existing, err)
		}
		if wantNet.String() == gotNet.String() {
			slog.Info("Docker 'kind' network already exists with correct CIDR", "cidr", cidr)
			return nil
		}
		return fmt.Errorf(
			"docker network 'kind' already exists with CIDR %q (want %q)\n"+
				"To fix: run 'kind delete clusters --all' inside the VM, then 'docker network rm kind'",
			existing, cidr,
		)
	}

	gateway, err := firstHostIP(cidr)
	if err != nil {
		return err
	}

	slog.Info("Creating Docker 'kind' network", "cidr", cidr, "gateway", gateway)
	cmd := fmt.Sprintf(
		"docker network create -d bridge --subnet=%s --gateway=%s "+
			"-o com.docker.network.bridge.enable_ip_masquerade=true "+
			"-o com.docker.network.driver.mtu=1500 kind",
		cidr, gateway,
	)
	_, err = g.Run(ctx, cmd)
	return err
}

func inspectKindSubnet(ctx context.Context, g *guest.Client) (string, error) {
	out, err := g.Run(ctx, "docker network inspect kind --format '{{json .IPAM.Config}}' 2>/dev/null || true")
	if err != nil {
		return "", fmt.Errorf("inspecting docker kind network: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "null" || out == "[]" {
		return "", nil
	}

	var configs []struct {
		Subnet string `json:"Subnet"`
	}
	if err := json.Unmarshal([]byte(out), &configs); err != nil {
		return "", fmt.Errorf("parsing docker network IPAM config: %w", err)
	}
	if len(configs) == 0 || configs[0].Subnet == "" {
		return "", nil
	}
	return configs[0].Subnet, nil
}

// firstHostIP returns the gateway IP (first host address) for a CIDR.
// e.g. "172.30.0.0/16" → "172.30.0.1"
func firstHostIP(cidr string) (string, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parsing CIDR %q: %w", cidr, err)
	}
	// Start from the network address, increment to first host.
	ip = ip.Mask(ipNet.Mask)
	ip[len(ip)-1]++
	return ip.String(), nil
}
