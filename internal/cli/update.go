package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/update"
	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Sync upstream changes and rebuild from source",
		Long: `Fetch upstream/main, rebase local custom commits on top, push to origin,
then build and install the updated binary.

Set NM_REPO_DIR to the repo path, or run this command from inside the repo.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("update", func() error {
				return update.Run(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), update.RunOptions{Stdin: cmd.InOrStdin()})
			})
		},
	}
	return cmd
}
