//go:build darwin

package main

// Blank-import the VZ driver so its init() registers it in Lima's driver
// registry before any VM operations are attempted. This mirrors what limactl
// does in its own main_darwin.go. Without this import the error
// "vmType vz is not a registered driver" is returned at VM creation time.
import _ "github.com/lima-vm/lima/v2/pkg/driver/vz"
