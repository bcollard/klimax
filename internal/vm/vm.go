package vm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/limatemplate"
	"github.com/lima-vm/lima/v2/pkg/instance"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/store"
	"gopkg.in/yaml.v3"
)

// Manager handles the Lima VM lifecycle for a named klimax instance.
type Manager struct {
	name       string
	klimaxHome string // ~/.klimax — used to locate the cached guest agent
}

// New creates a Manager for the given Lima instance name.
func New(name, klimaxHome string) *Manager {
	return &Manager{name: name, klimaxHome: klimaxHome}
}

// EnsureRunning is idempotent: creates the instance if it doesn't exist,
// starts it if it's stopped, and returns the running instance.
// When showLogs is true, Lima's host-agent logs are streamed to stderr in
// real time and Lima's built-in cloud-init progress display is enabled.
func (m *Manager) EnsureRunning(ctx context.Context, cfg *config.Config, showLogs bool) (*limatype.Instance, error) {
	inst, err := store.Inspect(ctx, m.name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspecting instance %q: %w", m.name, err)
	}

	if errors.Is(err, os.ErrNotExist) {
		slog.Info("Creating Lima instance", "name", m.name)
		inst, err = m.create(ctx, cfg)
		if err != nil {
			return nil, err
		}
	}

	if inst.Status == limatype.StatusRunning {
		slog.Info("Lima instance already running", "name", m.name)
		return inst, nil
	}

	slog.Info("Starting Lima instance", "name", m.name, "status", inst.Status)

	logCtx, cancelLogs := context.WithCancel(ctx)
	defer cancelLogs()
	if showLogs {
		go tailLimaLog(logCtx, filepath.Join(inst.Dir, "ha.stdout.log"), "lima")
		go tailLimaLog(logCtx, filepath.Join(inst.Dir, "ha.stderr.log"), "lima-err")
	}

	guestAgent, err := EnsureGuestAgent(ctx, m.klimaxHome)
	if err != nil {
		return nil, fmt.Errorf("guest agent: %w", err)
	}

	if err := instance.StartWithPaths(ctx, inst, false, showLogs, "", guestAgent); err != nil {
		return nil, fmt.Errorf("starting instance %q: %w", m.name, err)
	}
	cancelLogs()

	// Re-inspect to get updated SSH port and address.
	inst, err = store.Inspect(ctx, m.name)
	if err != nil {
		return nil, fmt.Errorf("re-inspecting instance after start: %w", err)
	}
	return inst, nil
}

// tailLimaLog waits for path to appear then streams each new line to stderr
// with the given prefix until ctx is cancelled.
func tailLimaLog(ctx context.Context, path, prefix string) {
	var f *os.File
	for f == nil {
		var err error
		f, err = os.Open(path)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for {
		for scanner.Scan() {
			fmt.Fprintf(os.Stderr, "[%s] %s\n", prefix, scanner.Text())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop gracefully shuts down the VM.
func (m *Manager) Stop(ctx context.Context) error {
	inst, err := store.Inspect(ctx, m.name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("Instance does not exist, nothing to stop", "name", m.name)
			return nil
		}
		return fmt.Errorf("inspecting instance %q: %w", m.name, err)
	}
	if inst.Status != limatype.StatusRunning {
		slog.Info("Instance is not running, nothing to stop", "name", m.name, "status", inst.Status)
		return nil
	}
	slog.Info("Stopping Lima instance", "name", m.name)
	if err := instance.StopGracefully(ctx, inst, false); err != nil {
		return fmt.Errorf("stopping instance %q: %w", m.name, err)
	}
	return nil
}

// Delete removes the VM and all its data. The instance must be stopped first
// (or pass force=true, which forcibly kills it).
func (m *Manager) Delete(ctx context.Context) error {
	inst, err := store.Inspect(ctx, m.name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("Instance does not exist, nothing to delete", "name", m.name)
			return nil
		}
		return fmt.Errorf("inspecting instance %q: %w", m.name, err)
	}
	slog.Info("Deleting Lima instance", "name", m.name)
	if err := instance.Delete(ctx, inst, true); err != nil {
		return fmt.Errorf("deleting instance %q: %w", m.name, err)
	}
	return nil
}

// Inspect returns the current state of the instance.
// Returns (nil, nil) if the instance does not exist.
func (m *Manager) Inspect(ctx context.Context) (*limatype.Instance, error) {
	inst, err := store.Inspect(ctx, m.name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspecting instance %q: %w", m.name, err)
	}
	return inst, nil
}

// create builds the Lima YAML from cfg and calls instance.Create.
func (m *Manager) create(ctx context.Context, cfg *config.Config) (*limatype.Instance, error) {
	y := limatemplate.Build(cfg)
	yamlBytes, err := yaml.Marshal(y)
	if err != nil {
		return nil, fmt.Errorf("marshaling Lima YAML: %w", err)
	}
	slog.Debug("Lima YAML", "yaml", string(yamlBytes))

	inst, err := instance.Create(ctx, m.name, yamlBytes, false)
	if err != nil {
		return nil, fmt.Errorf("creating Lima instance %q: %w", m.name, err)
	}

	// instance.Create writes lima-version using Lima's internal version.Version,
	// which is "<unknown>" when built without -ldflags. Replace it with the
	// actual Lima module version so store.Inspect doesn't emit a warning on every run.
	// os.Remove + os.WriteFile is used because the file is created 0o444 (read-only);
	// removing a file only requires write permission on the parent directory.
	limaVerFile := filepath.Join(inst.Dir, "lima-version")
	_ = os.Remove(limaVerFile)
	_ = os.WriteFile(limaVerFile, []byte(limaModuleVersion()), 0o444)

	return inst, nil
}
