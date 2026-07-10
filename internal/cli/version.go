package cli

import (
	"github.com/spf13/cobra"
	"github.com/tarik02/webdesktop/internal/version"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Println(version.Details())
		},
	}
}
