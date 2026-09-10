package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcollard/klimax/internal/config"
)

func TestCheckMountLocations(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mounts []config.Mount
		want   string // substring; empty means the config must be accepted
	}{
		{
			name:   "no mounts",
			mounts: nil,
		},
		{
			name:   "existing directory",
			mounts: []config.Mount{{Location: dir, Writable: true}},
		},
		{
			name:   "missing directory",
			mounts: []config.Mount{{Location: filepath.Join(dir, "nope")}},
			want:   "does not exist",
		},
		{
			// A file is not shareable, and Lima's own error for it is obscure.
			name:   "location is a file",
			mounts: []config.Mount{{Location: file}},
			want:   "is not a directory",
		},
		{
			// Every bad entry is reported, not just the first.
			name: "reports each bad entry",
			mounts: []config.Mount{
				{Location: filepath.Join(dir, "nope")},
				{Location: file},
			},
			want: "does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.VM.Mounts = tt.mounts
			err := checkMountLocations(cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("checkMountLocations = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkMountLocations accepted %+v, want error", tt.mounts)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should mention %q, got: %v", tt.want, err)
			}
		})
	}

	// The multi-error case must name both problems, so one fix does not just
	// reveal the next.
	cfg := &config.Config{}
	cfg.VM.Mounts = []config.Mount{
		{Location: filepath.Join(dir, "nope")},
		{Location: file},
	}
	err := checkMountLocations(cfg)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("both problems should be reported, got: %v", err)
	}
}
