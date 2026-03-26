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
// It first removes any existing route for that CIDR (best-effort), then adds it.
// Requires sudo; macOS will prompt if not already authorized.
func EnsureRoute(cidr, lima0IP string) error {
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

