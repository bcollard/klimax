package cli

import (
	"os"

	"github.com/spf13/cobra"
)

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell auto-completion script",
		Long: `Generate an auto-completion script for klimax for the specified shell.

To load completions for the current session only:

  bash:        source <(klimax completion bash)
  zsh:         source <(klimax completion zsh)
  fish:        klimax completion fish | source
  powershell:  klimax completion powershell | Out-String | Invoke-Expression

To load completions permanently:

  bash (macOS via Homebrew bash-completion):
    klimax completion bash > $(brew --prefix)/etc/bash_completion.d/klimax

  bash (Linux):
    klimax completion bash > /etc/bash_completion.d/klimax

  zsh:
    echo "autoload -U compinit; compinit" >> ~/.zshrc
    klimax completion zsh > "${fpath[1]}/_klimax"

  fish:
    klimax completion fish > ~/.config/fish/completions/klimax.fish

  powershell:
    klimax completion powershell >> $PROFILE
`,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(os.Stdout)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			}
			return nil
		},
	}
	return cmd
}
