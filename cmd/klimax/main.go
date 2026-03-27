package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bcollard/klimax/internal/cli"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	// Keep all Lima state under ~/.klimax instead of the default ~/.lima,
	// so klimax instances are isolated from other Lima users (limactl, colima…).
	// LIMA_HOME is read by Lima at call time (not at init), so setting it
	// here — before root.Execute() — is sufficient.
	if home, err := os.UserHomeDir(); err == nil {
		os.Setenv("LIMA_HOME", filepath.Join(home, ".klimax"))
	} else {
		fmt.Fprintf(os.Stderr, "warning: cannot determine home directory: %v\n", err)
	}

	root := cli.NewRootCmd(version)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
