package main

import (
	"os"

	"github.com/bcollard/klimax/internal/cli"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	root := cli.NewRootCmd(version)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
