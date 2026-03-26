package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/limatemplate"
	"github.com/lima-vm/lima/v2/pkg/instance"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/store"
	"gopkg.in/yaml.v3"
)

// Manager handles the Lima VM lifecycle for a named klimax instance.
type Manager struct {
	name string
}

// New creates a Manager for the given Lima instance name.
func New(name string) *Manager {
	return &Manager{name: name}
}

// EnsureRunning is idempotent: creates the instance if it doesn't exist,
// starts it if it's stopped, and returns the running instance.
func (m *Manager) EnsureRunning(ctx context.Context, cfg *config.Config) (*limatype.Instance, error) {
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
	if err := instance.Start(ctx, inst, false, true); err != nil {
		return nil, fmt.Errorf("starting instance %q: %w", m.name, err)
	}

	// Re-inspect to get updated SSH port and address.
	inst, err = store.Inspect(ctx, m.name)
	if err != nil {
		return nil, fmt.Errorf("re-inspecting instance after start: %w", err)
	}
	return inst, nil
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
	return inst, nil
}
