package cli

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/bcollard/klimax/internal/localca"
	"github.com/bcollard/klimax/internal/localdns"
)

// A fleet has its own DNS zone, <fleet>.<domain>, beside its members' zones.
// Fleet-wide names (gateway.<fleet>.<domain>) are published by whichever member
// annotates a Service with one; klimax only has to let every member write
// there, give the zone a wildcard certificate, and clean up after a member.
//
// Membership is the live klimax.dev/fleet node label, as everywhere else in
// klimax — never the manifest.

// liveFleetOf returns the fleet a cluster belongs to, or "" when it has none or
// its API cannot be read.
func liveFleetOf(ctx context.Context, g *guest.Client, cluster string) string {
	info, err := kind.ClusterInfoFor(ctx, g, cluster)
	if err != nil || info == nil {
		return ""
	}
	return info.Labels[fleetLabelKey]
}

// checkZoneNames refuses a fleet and a cluster sharing a name: both would own
// <name>.<domain>, and neither the DNS records nor the CA's name constraints
// could tell them apart.
func checkZoneNames(ctx context.Context, g *guest.Client, cfg *config.Config, fleet string, clusters ...string) error {
	if !cfg.DNSEnabled() || fleet == "" {
		return nil
	}
	if slices.Contains(clusters, fleet) {
		return fmt.Errorf("fleet %q and a cluster share a name — both would own %s; rename one", fleet, cfg.FleetDNSZone(fleet))
	}
	live, err := kind.ListClusters(ctx, g)
	if err != nil {
		return err
	}
	if slices.Contains(live, fleet) {
		return fmt.Errorf("fleet %q has the name of an existing cluster — both would own %s; rename the fleet", fleet, cfg.FleetDNSZone(fleet))
	}
	return nil
}

// checkClusterNameFree refuses a new cluster named like an existing fleet.
func checkClusterNameFree(ctx context.Context, g *guest.Client, cfg *config.Config, cluster string) error {
	if !cfg.DNSEnabled() {
		return nil
	}
	fleets, err := kind.ClustersByFleet(ctx, g)
	if err != nil {
		return nil // advisory: a read failure must not block cluster creation
	}
	if _, taken := fleets[cluster]; taken && cluster != "" {
		return fmt.Errorf("cluster %q has the name of an existing fleet — both would own %s; rename the cluster", cluster, cfg.FleetDNSZone(cluster))
	}
	return nil
}

// installFleetCA installs a fleet's wildcard (and, with cert-manager, its
// ClusterIssuer) into one member. A failure is a warning: the member works.
func installFleetCA(ctx context.Context, g *guest.Client, cfg *config.Config, cluster, fleet string) {
	if fleet == "" || !cfg.TLSEnabled() {
		return
	}
	fm, err := caStore(cfg).EnsureFleet(fleet)
	if err == nil {
		_, err = localca.InstallInCluster(ctx, g, cluster, fm)
	}
	if err != nil {
		slog.Warn("Local CA: could not install the fleet's certificates", "cluster", cluster, "fleet", fleet, "err", err,
			"fix", "klimax ca attach "+cluster)
		return
	}
	fmt.Printf("tls: fleet wildcard %s in Secret default/%s\n", fm.WildcardNames[0], localca.FleetWildcardSecret)
}

// joinFleetZone gives a cluster that just became a fleet member (adopt) the
// fleet's DNS zone and certificates. Its own zone and node trust are
// unchanged.
func joinFleetZone(ctx context.Context, g *guest.Client, cfg *config.Config, cluster, fleet string) {
	installLocalDNS(ctx, g, cfg, cluster, fleet)
	installFleetCA(ctx, g, cfg, cluster, fleet)
}

// leaveFleetZone cleans up after a deleted member: the fleet-zone records it
// published, and — once the fleet has no members left — the zone itself and
// the fleet's CA material.
func leaveFleetZone(ctx context.Context, g *guest.Client, cfg *config.Config, cluster, fleet string) {
	if fleet == "" {
		return
	}
	if cfg.DNSEnabled() {
		if err := localdns.PurgeOwned(ctx, g, cfg.FleetDNSZone(fleet), cluster); err != nil {
			slog.Warn("Could not remove the cluster's fleet-zone records", "cluster", cluster, "fleet", fleet, "err", err)
		}
	}
	byFleet, err := kind.ClustersByFleet(ctx, g)
	if err != nil || len(byFleet[fleet]) > 0 {
		return
	}
	slog.Info("Last member of the fleet deleted — removing its DNS zone and certificates", "fleet", fleet)
	if cfg.DNSEnabled() {
		_ = localdns.PurgeZone(ctx, g, cfg.FleetDNSZone(fleet))
	}
	if err := caStore(cfg).RemoveFleet(fleet); err != nil {
		slog.Warn("Could not remove the fleet's CA material", "fleet", fleet, "err", err)
	}
}
