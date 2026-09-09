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
	"github.com/lima-vm/lima/v2/pkg/limatype"
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

// ResizeImageDisk grows the persistent Lima data disk backing the guest's
// container image store to size (e.g. "30GiB"). It fails if the disk does not
// exist yet (nothing to resize — EnsureImageDisk creates it at the next
// 'klimax up'), if size is smaller than the disk's current size (shrinking a
// live filesystem is unsafe), or if it is currently attached to a running VM
// (Lima's own disk resize has the same restriction — the backing file must be
// closed).
//
// Resizing here only grows the backing image file; the guest's ext4
// filesystem on top of it is grown separately by klimax-image-disk.sh on the
// next boot (see imageDiskProvisions in internal/limatemplate).
func ResizeImageDisk(ctx context.Context, name, size string) error {
	newBytes, err := units.RAMInBytes(size)
	if err != nil {
		return fmt.Errorf("invalid size %q (use e.g. 30GiB): %w", size, err)
	}

	disk, err := store.InspectDisk(name, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("image disk %q does not exist yet — it is created at the size configured in imageDisk on the next 'klimax up'", name)
		}
		return fmt.Errorf("inspecting image disk %q: %w", name, err)
	}

	if newBytes < disk.Size {
		return fmt.Errorf("cannot shrink the image disk: current size is %s, requested %s", units.BytesSize(float64(disk.Size)), size)
	}
	if newBytes == disk.Size {
		slog.Info("Image disk already at requested size — nothing to do", "name", name, "size", size)
		return nil
	}

	if disk.Instance != "" {
		if inst, ierr := store.Inspect(ctx, disk.Instance); ierr == nil && inst.Status == limatype.StatusRunning {
			return fmt.Errorf("cannot resize image disk %q: VM %q is running — stop it first (klimax down)", name, disk.Instance)
		}
	}

	dataDisk := filepath.Join(disk.Dir, filenames.DataDisk)
	if err := proxyimgutil.NewDiskUtil(ctx).ResizeDisk(ctx, dataDisk, newBytes); err != nil {
		return fmt.Errorf("resizing image disk %q: %w", name, err)
	}
	slog.Info("Resized image disk", "name", name, "size", size, "dir", disk.Dir)
	return nil
}

// ImageDiskSize returns the current size in bytes of the image disk backing the
// guest's container image store, and whether it exists yet.
func ImageDiskSize(name string) (int64, bool, error) {
	disk, err := store.InspectDisk(name, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("inspecting image disk %q: %w", name, err)
	}
	return disk.Size, true, nil
}
