package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the klimax version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("klimax %s\n", version)
		},
	}
}
