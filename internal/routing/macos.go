package routing

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"

	"github.com/bcollard/klimax/internal/guest"
)

// EnsureRoute idempotently adds a macOS route for cidr via the VM's lima0 IP.
// If a route for that CIDR already points at lima0IP it is a no-op — importantly
// it does NOT invoke sudo, so re-running `klimax up` against an already-running VM
// never prompts for a password. Only a missing or stale (wrong-gateway) route
// triggers the sudo delete+add.
func EnsureRoute(cidr, lima0IP string) error {
	if gw, ok := RouteGateway(cidr); ok {
		if gw == lima0IP {
			slog.Info("macOS route already present and correct — skipping", "cidr", cidr, "via", lima0IP)
			return nil
		}
		slog.Info("macOS route has a stale gateway — refreshing", "cidr", cidr, "old", gw, "new", lima0IP)
	}

	slog.Info("Ensuring macOS route", "cidr", cidr, "via", lima0IP)

	// Best-effort delete of any stale route.
	del := exec.Command("sudo", "/sbin/route", "-n", "delete", "-net", cidr)
	_ = del.Run() // ignore errors (route may not exist)

	add := exec.Command("sudo", "/sbin/route", "-n", "add", "-net", cidr, lima0IP)
	slog.Debug("route add", "cmd", strings.Join(add.Args, " "))
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("adding route %s via %s: %w\n%s", cidr, lima0IP, err, out)
	}
	return nil
}

// RouteGateway returns the gateway of the macOS route that currently serves the
// given CIDR, and whether a route dedicated to that CIDR exists. It returns
// ok=false when only a broader/default route matches, so the caller knows a
// dedicated route still needs to be added. It performs no privileged operations
// (no sudo), making it safe to call on every `klimax up`.
func RouteGateway(cidr string) (string, bool) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", false
	}
	base := ip.Mask(ipNet.Mask)
	probe := make(net.IP, len(base))
	copy(probe, base)
	probe[len(probe)-1]++

	out, err := exec.Command("/sbin/route", "-n", "get", probe.String()).CombinedOutput()
	if err != nil {
		return "", false
	}
	var dest, gw string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "destination:"):
			dest = strings.TrimSpace(strings.TrimPrefix(line, "destination:"))
		case strings.HasPrefix(line, "gateway:"):
			gw = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
	}
	// Only "our" route if the matched destination is the CIDR base (not the
	// default route or a broader aggregate).
	if dest != base.String() || gw == "" {
		return "", false
	}
	return gw, true
}

// DeleteRoute removes the macOS route for cidr. Best-effort; ignores "not found".
func DeleteRoute(cidr string) error {
	slog.Info("Deleting macOS route", "cidr", cidr)
	del := exec.Command("sudo", "/sbin/route", "-n", "delete", "-net", cidr)
	if out, err := del.CombinedOutput(); err != nil {
		slog.Warn("route delete failed (may not exist)", "cidr", cidr, "err", err, "output", string(out))
	}
	return nil
}

// RouteExists checks whether a macOS route for the given CIDR currently exists.
func RouteExists(cidr string) bool {
	// Pick the first host IP in the CIDR to query.
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	ip = ip.Mask(ipNet.Mask)
	ip[len(ip)-1]++
	cmd := exec.Command("/sbin/route", "-n", "get", ip.String())
	return cmd.Run() == nil
}

// Lima0IP returns the IPv4 address of the lima0 interface inside the VM.
// This is the gateway the macOS host must route kind traffic through.
func Lima0IP(ctx context.Context, g *guest.Client) (string, error) {
	out, err := g.Run(ctx, `ip -o -4 addr show lima0 | awk '{print $4}' | cut -d/ -f1`)
	if err != nil {
		return "", fmt.Errorf("detecting lima0 IP: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("lima0 interface has no IPv4 address")
	}
	if ip := net.ParseIP(out); ip == nil {
		return "", fmt.Errorf("invalid IP from lima0: %q", out)
	}
	return out, nil
}

