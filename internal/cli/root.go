package cli

import (
	"github.com/spf13/cobra"
	"github.com/tarik02/webdesktop/internal/version"
)

// Execute runs the webdesktop command.
func Execute() error {
	return newRootCommand().Execute()
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "webdesktop",
		Short:         "KDE Wayland desktop capture service",
		Version:       version.Short(),
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	cmd.AddCommand(
		newServeCommand(),
		newVersionCommand(),
	)
	return cmd
}
