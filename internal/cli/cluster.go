package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cluster",
		Aliases: []string{"cl"},
		Short:   "Manage kind clusters inside the klimax VM",
	}
	cmd.AddCommand(
		newClusterCreateCmd(),
		newClusterDeleteCmd(),
		newClusterListCmd(),
		newClusterUseCmd(),
		newClusterMergeCmd(),
		newClusterE2ETestNginxCmd(),
	)
	return cmd
}

// ─── create ──────────────────────────────────────────────────────────────────

func newClusterCreateCmd() *cobra.Command {
	var num int
	var region, zone string
	cmd := &cobra.Command{
		Use:     "create <name>",
		Aliases: []string{"cr"},
		Short:   "Create a new kind cluster in the running VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterCreate(cmd.Context(), args[0], num, region, zone)
		},
	}
	cmd.Flags().IntVar(&num, "num", 0, "Cluster number (1-99) for subnet/port allocation; auto-assigned if 0")
	cmd.Flags().StringVar(&region, "region", "", "topology.kubernetes.io/region label (default: europe-west<N>)")
	cmd.Flags().StringVar(&zone, "zone", "", "topology.kubernetes.io/zone label (default: europe-west<N>-b)")
	return cmd
}

func runClusterCreate(ctx context.Context, name string, num int, region, zone string) error {
	cfg, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	// Resolve num: find the lowest free slot by inspecting live cluster port bindings.
	if num == 0 {
		usedNums, err := kind.DetectUsedNums(ctx, g)
		if err != nil {
			return fmt.Errorf("detecting used cluster nums: %w", err)
		}
		num, err = kind.NextFreeNum(usedNums)
		if err != nil {
			return err
		}
		slog.Info("Auto-assigned cluster num", "name", name, "num", num)
	}

	cl := config.ClusterConfig{Name: name, Num: num, Region: region, Zone: zone}

	if err := kind.CreateCluster(ctx, g, cl, cfg.Kind, cfg.Registries, cfg.Network.KindBridgeCIDR); err != nil {
		return err
	}

	if *cfg.Kind.AutoMergeKubeconfig {
		if err := runClusterMerge(name); err != nil {
			slog.Warn("Auto-merge kubeconfig failed", "cluster", name, "err", err)
		}
	}
	return nil
}

// ─── delete ──────────────────────────────────────────────────────────────────

func newClusterDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete [name]",
		Aliases: []string{"de"},
		Short:   "Delete a kind cluster (interactive picker when no name is given)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runClusterDelete(cmd.Context(), args[0])
			}
			return runClusterDeleteInteractive(cmd.Context())
		},
	}
}

func runClusterDelete(ctx context.Context, name string) error {
	cfg, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}
	if err := kind.DeleteCluster(ctx, g, name); err != nil {
		return err
	}
	if *cfg.Kind.AutoRemoveKubeconfig {
		if err := removeFromKubeconfig(name); err != nil {
			slog.Warn("Auto-remove kubeconfig failed", "cluster", name, "err", err)
		}
	}
	return nil
}

// runClusterDeleteInteractive shows a multi-select picker and deletes chosen clusters.
func runClusterDeleteInteractive(ctx context.Context) error {
	cfg, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	names, err := kind.ListClusters(ctx, g)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("No kind clusters found.")
		return nil
	}

	usedNums, _ := kind.DetectUsedNums(ctx, g) // best-effort; port 0 if unknown
	numByName := make(map[string]int, len(usedNums))
	for num, name := range usedNums {
		numByName[name] = num
	}

	items := make([]pickerItem, len(names))
	for i, n := range names {
		items[i] = pickerItem{name: n, apiPort: 7000 + numByName[n]}
	}

	selected, err := runPicker(items)
	if err != nil || len(selected) == 0 {
		return err
	}

	for _, name := range selected {
		fmt.Printf("Deleting cluster %q...\n", name)
		if err := kind.DeleteCluster(ctx, g, name); err != nil {
			slog.Warn("Delete failed", "cluster", name, "err", err)
			continue
		}
		if *cfg.Kind.AutoRemoveKubeconfig {
			if err := removeFromKubeconfig(name); err != nil {
				slog.Warn("Auto-remove kubeconfig failed", "cluster", name, "err", err)
			}
		}
	}
	return nil
}

// pickerItem is a cluster entry shown in the interactive delete picker.
type pickerItem struct {
	name    string
	apiPort int
}

// runPicker renders a keyboard-driven multi-select list in raw terminal mode.
// Returns the names of selected items, or nil if cancelled.
func runPicker(items []pickerItem) ([]string, error) {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, fmt.Errorf("enabling raw terminal mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState) //nolint:errcheck

	cursor := 0
	sel := make([]bool, len(items))
	firstDraw := true

	draw := func() {
		if !firstDraw {
			// Move up to overwrite the previous render (header + blank + items + blank + footer).
			fmt.Printf("\033[%dA", len(items)+4)
		}
		firstDraw = false

		fmt.Print("\033[2KDelete kind clusters (↑/↓ navigate · Space toggle · a=all · Enter confirm · q quit)\r\n")
		fmt.Print("\033[2K\r\n")
		for i, item := range items {
			check := "[ ]"
			if sel[i] {
				check = "[x]"
			}
			prefix := "  "
			if i == cursor {
				prefix = "> "
			}
			port := ""
			if item.apiPort > 7000 {
				port = fmt.Sprintf("  port %d", item.apiPort)
			}
			fmt.Printf("\033[2K  %s%s  %-30s%s\r\n", prefix, check, item.name, port)
		}
		fmt.Print("\033[2K\r\n")
		n := 0
		for _, v := range sel {
			if v {
				n++
			}
		}
		if n == 0 {
			fmt.Print("\033[2K  nothing selected\r\n")
		} else {
			fmt.Printf("\033[2K  %d cluster(s) selected — press Enter to delete\r\n", n)
		}
	}

	buf := make([]byte, 4)
	draw()
	for {
		nr, _ := os.Stdin.Read(buf)
		if nr == 0 {
			continue
		}
		switch {
		case buf[0] == 'q':
			fmt.Print("\r\nCancelled.\r\n")
			return nil, nil
		case buf[0] == 27 && nr == 1: // bare Escape
			fmt.Print("\r\nCancelled.\r\n")
			return nil, nil
		case buf[0] == 13: // Enter
			var result []string
			for i, v := range sel {
				if v {
					result = append(result, items[i].name)
				}
			}
			fmt.Print("\r\n")
			return result, nil
		case buf[0] == ' ':
			sel[cursor] = !sel[cursor]
		case buf[0] == 'a':
			// Toggle all: select all if any are unselected, else deselect all.
			anyUnset := false
			for _, v := range sel {
				if !v {
					anyUnset = true
					break
				}
			}
			for i := range sel {
				sel[i] = anyUnset
			}
		case nr >= 3 && buf[0] == 27 && buf[1] == '[':
			switch buf[2] {
			case 'A': // up
				if cursor > 0 {
					cursor--
				}
			case 'B': // down
				if cursor < len(items)-1 {
					cursor++
				}
			}
		}
		draw()
	}
}

// ─── list ────────────────────────────────────────────────────────────────────

func newClusterListCmd() *cobra.Command {
	var outputFmt string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List kind clusters running in the VM",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterList(cmd.Context(), outputFmt)
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	return cmd
}

type clusterInfo struct {
	Name           string `json:"name"           yaml:"name"`
	Num            int    `json:"num"            yaml:"num"`
	APIPort        int    `json:"apiPort"        yaml:"apiPort"`
	KubeconfigPath string `json:"kubeconfigPath" yaml:"kubeconfigPath"`
}

func runClusterList(ctx context.Context, outputFmt string) error {
	_, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	names, err := kind.ListClusters(ctx, g)
	if err != nil {
		return err
	}

	// Detect nums from live port bindings.
	usedNums, _ := kind.DetectUsedNums(ctx, g) // best-effort; num=0 if unknown
	numToName := make(map[string]int, len(usedNums))
	for n, name := range usedNums {
		numToName[name] = n
	}

	clusters := make([]clusterInfo, 0, len(names))
	for _, n := range names {
		num := numToName[n]
		clusters = append(clusters, clusterInfo{
			Name:           n,
			Num:            num,
			APIPort:        7000 + num,
			KubeconfigPath: kind.KindKubeconfigPath(n),
		})
	}

	switch outputFmt {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(clusters)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(clusters)
	default: // "text"
		if len(names) == 0 {
			fmt.Println("No kind clusters found.")
			return nil
		}
		fmt.Printf("%-30s  %4s  %8s  %s\n", "NAME", "NUM", "API-PORT", "KUBECONFIG")
		fmt.Printf("%-30s  %4s  %8s  %s\n", strings.Repeat("-", 30), "----", "--------", "---------")
		for _, c := range clusters {
			fmt.Printf("%-30s  %4d  %8d  %s\n", c.Name, c.Num, c.APIPort, c.KubeconfigPath)
		}
		return nil
	}
}

// ─── use ─────────────────────────────────────────────────────────────────────

func newClusterUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Print the export command to set KUBECONFIG for the given cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := kind.KindKubeconfigPath(args[0])
			if _, err := os.Stat(path); os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: kubeconfig not found at %s\n", path)
			}
			fmt.Printf("export KUBECONFIG=%s\n", path)
			return nil
		},
	}
}

// ─── merge ───────────────────────────────────────────────────────────────────

func newClusterMergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <name>",
		Short: "Merge cluster kubeconfig into ~/.kube/config",
		Long: `Adds the cluster's context, cluster, and user entries into the default
kubeconfig (~/.kube/config) so that kubectx/kubens can switch to it.

A backup of the existing ~/.kube/config is written to ~/.kube/config.bak
before any modifications are made.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterMerge(args[0])
		},
	}
}

func runClusterMerge(name string) error {
	srcPath := kind.KindKubeconfigPath(name)
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("kubeconfig not found at %s — run 'klimax cluster create %s' first", srcPath, name)
	}

	srcData, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("reading cluster kubeconfig: %w", err)
	}
	var src kubeconfigFile
	if err := yaml.Unmarshal(srcData, &src); err != nil {
		return fmt.Errorf("parsing cluster kubeconfig: %w", err)
	}

	// Load or initialise the default kubeconfig.
	home, _ := os.UserHomeDir()
	dstPath := filepath.Join(home, ".kube", "config")
	var dst kubeconfigFile
	dstData, err := os.ReadFile(dstPath)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(dstData, &dst); err != nil {
			return fmt.Errorf("parsing ~/.kube/config: %w", err)
		}
		// Backup before any modification.
		bakPath := dstPath + ".bak"
		if err := os.WriteFile(bakPath, dstData, 0o600); err != nil {
			return fmt.Errorf("writing backup %s: %w", bakPath, err)
		}
		slog.Info("Backup written", "path", bakPath)
	case os.IsNotExist(err):
		dst = kubeconfigFile{APIVersion: "v1", Kind: "Config"}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
			return fmt.Errorf("creating ~/.kube: %w", err)
		}
	default:
		return fmt.Errorf("reading ~/.kube/config: %w", err)
	}

	dst.Clusters = mergeKubeconfigEntries(dst.Clusters, src.Clusters)
	dst.Contexts = mergeKubeconfigEntries(dst.Contexts, src.Contexts)
	dst.Users = mergeKubeconfigEntries(dst.Users, src.Users)

	out, err := yaml.Marshal(&dst)
	if err != nil {
		return fmt.Errorf("marshaling merged kubeconfig: %w", err)
	}
	if err := os.WriteFile(dstPath, out, 0o600); err != nil {
		return fmt.Errorf("writing ~/.kube/config: %w", err)
	}

	// Report the context name(s) that were merged in.
	contextName := "kind-" + name // kind's default naming convention
	if len(src.Contexts) > 0 {
		if n, ok := src.Contexts[0]["name"].(string); ok {
			contextName = n
		}
	}
	slog.Info("Kubeconfig merged", "context", contextName, "dst", dstPath)
	fmt.Printf("Merged context %q into %s\n  kubectx %s\n", contextName, dstPath, contextName)
	return nil
}

// kubeconfigFile is a minimal kubeconfig representation for faithful YAML round-trips.
type kubeconfigFile struct {
	APIVersion     string                   `yaml:"apiVersion"`
	Kind           string                   `yaml:"kind"`
	Clusters       []map[string]interface{} `yaml:"clusters"`
	Contexts       []map[string]interface{} `yaml:"contexts"`
	Users          []map[string]interface{} `yaml:"users"`
	CurrentContext string                   `yaml:"current-context,omitempty"`
	Preferences    interface{}              `yaml:"preferences,omitempty"`
}

// removeFromKubeconfig removes the context, cluster, and user entries for the
// given cluster name from ~/.kube/config. klimax strips the "kind-" prefix
// during exportKubeconfig, so entries are stored under the bare cluster name.
func removeFromKubeconfig(clusterName string) error {
	home, _ := os.UserHomeDir()
	dstPath := filepath.Join(home, ".kube", "config")
	data, err := os.ReadFile(dstPath)
	if os.IsNotExist(err) {
		return nil // nothing to do
	}
	if err != nil {
		return fmt.Errorf("reading ~/.kube/config: %w", err)
	}

	var kc kubeconfigFile
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return fmt.Errorf("parsing ~/.kube/config: %w", err)
	}

	entryName := clusterName
	kc.Clusters = removeKubeconfigEntry(kc.Clusters, entryName)
	kc.Contexts = removeKubeconfigEntry(kc.Contexts, entryName)
	kc.Users = removeKubeconfigEntry(kc.Users, entryName)
	if kc.CurrentContext == entryName {
		kc.CurrentContext = ""
	}

	out, err := yaml.Marshal(&kc)
	if err != nil {
		return fmt.Errorf("marshaling kubeconfig: %w", err)
	}
	if err := os.WriteFile(dstPath, out, 0o600); err != nil {
		return fmt.Errorf("writing ~/.kube/config: %w", err)
	}
	slog.Info("Kubeconfig entries removed", "context", entryName, "dst", dstPath)
	return nil
}

func removeKubeconfigEntry(entries []map[string]interface{}, name string) []map[string]interface{} {
	out := entries[:0:0]
	for _, e := range entries {
		if n, _ := e["name"].(string); n != name {
			out = append(out, e)
		}
	}
	return out
}

// mergeKubeconfigEntries merges src into dst, overwriting entries with the same "name".
func mergeKubeconfigEntries(dst, src []map[string]interface{}) []map[string]interface{} {
	idx := make(map[string]int, len(dst))
	for i, e := range dst {
		if n, ok := e["name"].(string); ok {
			idx[n] = i
		}
	}
	for _, e := range src {
		n, _ := e["name"].(string)
		if i, exists := idx[n]; exists {
			dst[i] = e
		} else {
			idx[n] = len(dst)
			dst = append(dst, e)
		}
	}
	return dst
}

// ─── e2e-test-nginx ──────────────────────────────────────────────────────────

const nginxImage = "nginx:1.27"

func newClusterE2ETestNginxCmd() *cobra.Command {
	var cleanup bool
	cmd := &cobra.Command{
		Use:   "e2e-test-nginx",
		Short: "Deploy nginx and curl it via LoadBalancer to verify the cluster end-to-end",
		Long: `Deploys nginx:1.27 in the default namespace using the current kubectl context,
exposes it as a LoadBalancer service, waits for MetalLB to assign an IP, and curls it.

Cleans up existing nginx pod/svc before starting so the test is idempotent.
Use --cleanup to only remove the nginx pod and service without running the test.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterE2ETestNginx(cmd.Context(), cleanup)
		},
	}
	cmd.Flags().BoolVar(&cleanup, "cleanup", false, "Delete the nginx pod and service instead of running the test")
	return cmd
}

func runClusterE2ETestNginx(ctx context.Context, cleanup bool) error {
	kubectl := func(args ...string) error {
		c := exec.CommandContext(ctx, "kubectl", args...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}

	if cleanup {
		fmt.Println("--- Cleaning up nginx resources ---")
		_ = kubectl("delete", "pod", "nginx", "--ignore-not-found")
		_ = kubectl("delete", "svc", "nginx", "--ignore-not-found")
		return nil
	}

	// Clean up any previous test resources (idempotent).
	_ = kubectl("delete", "pod", "nginx", "--ignore-not-found")
	_ = kubectl("delete", "svc", "nginx", "--ignore-not-found")

	fmt.Printf("--- Deploying %s ---\n", nginxImage)
	if err := kubectl("run", "nginx", "--image", nginxImage); err != nil {
		return fmt.Errorf("kubectl run: %w", err)
	}

	fmt.Println("--- Exposing as LoadBalancer ---")
	if err := kubectl("expose", "pod", "nginx", "--port", "80", "--type", "LoadBalancer"); err != nil {
		return fmt.Errorf("kubectl expose: %w", err)
	}

	if err := kubectl("get", "svc"); err != nil {
		return fmt.Errorf("kubectl get svc: %w", err)
	}

	fmt.Println("--- Waiting for pod ready (60s) ---")
	if err := kubectl("wait", "pod", "nginx", "--for=condition=Ready", "--timeout=60s"); err != nil {
		return fmt.Errorf("pod did not become ready: %w", err)
	}

	fmt.Println("--- Waiting for LoadBalancer IP (up to 60s) ---")
	var lbIP string
	for range 30 {
		c := exec.CommandContext(ctx, "kubectl", "get", "svc", "nginx",
			"-o", `jsonpath={.status.loadBalancer.ingress[0].ip}`)
		out, _ := c.Output()
		if ip := strings.TrimSpace(string(out)); ip != "" {
			lbIP = ip
			break
		}
		time.Sleep(2 * time.Second)
	}
	if lbIP == "" {
		return fmt.Errorf("timed out waiting for LoadBalancer IP")
	}

	fmt.Printf("--- Curling http://%s ---\n", lbIP)
	c := exec.CommandContext(ctx, "curl", "--max-time", "11", "--connect-timeout", "10", "-I", "-v",
		"http://"+lbIP)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("curl failed: %w", err)
	}

	fmt.Println("--- e2e test passed ---")
	return nil
}

// ─── shared helpers ──────────────────────────────────────────────────────────

// connectToRunningVM loads config and opens an SSH client to the running VM.
func connectToRunningVM(ctx context.Context) (*config.Config, *guest.Client, error) {
	cfg, err := loadAndValidate()
	if err != nil {
		return nil, nil, err
	}

	mgr := vm.New(cfg.VM.Name, KlimaxHome())
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("inspecting VM: %w", err)
	}
	if inst == nil {
		return nil, nil, errors.New("VM does not exist; run 'klimax up' first")
	}

	g, err := guest.NewClient(inst)
	if err != nil {
		return nil, nil, fmt.Errorf("opening SSH client: %w", err)
	}
	return cfg, g, nil
}
