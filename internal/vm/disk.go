package vm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/docker/go-units"
	"github.com/lima-vm/lima/v2/pkg/imgutil/proxyimgutil"
	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"
	"github.com/lima-vm/lima/v2/pkg/store"
)

// EnsureImageDisk creates the persistent Lima data disk backing the guest's
// container image store (/var/lib/containerd), if it does not already exist.
//
// Named Lima disks live in $LIMA_HOME/_disk/<name> — outside the instance
// directory — and deliberately survive `klimax destroy`. That is the whole
// point: `destroy` otherwise discards the unpacked image store (kindest/node,
// registry:2, and any locally built images, which no mirror can restore).
//
// Lima itself partitions and formats the disk on first attach (Disk.Format),
// and unlocks it when the instance stops, so re-creating the VM re-attaches
// the same disk with its contents intact.
func EnsureImageDisk(ctx context.Context, name, size string) error {
	diskSize, err := units.RAMInBytes(size)
	if err != nil {
		return fmt.Errorf("parsing vm.imageDisk size %q: %w", size, err)
	}

	diskDir, err := store.DiskDir(name)
	if err != nil {
		return err
	}

	switch _, err := os.Stat(diskDir); {
	case err == nil:
		slog.Debug("Persistent image disk already exists", "name", name, "dir", diskDir)
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("inspecting image disk %q: %w", name, err)
	}

	slog.Info("Creating persistent image disk", "name", name, "size", size, "dir", diskDir)
	if err := os.MkdirAll(diskDir, 0o700); err != nil {
		return err
	}

	dataDisk := filepath.Join(diskDir, filenames.DataDisk)
	if err := proxyimgutil.NewDiskUtil(ctx).CreateDisk(ctx, dataDisk, diskSize); err != nil {
		// Don't leave a half-created disk dir behind; Lima would treat it as valid.
		if rerr := os.RemoveAll(diskDir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing %q: %w", diskDir, rerr))
		}
		return fmt.Errorf("creating image disk %q: %w", name, err)
	}
	return nil
}
