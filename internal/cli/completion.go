package cli

import (
	"os"

	"github.com/spf13/cobra"
)

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell auto-completion script",
		Long: `Generate an auto-completion script for marina for the specified shell.

To load completions for the current session only:

  bash:        source <(marina completion bash)
  zsh:         source <(marina completion zsh)
  fish:        marina completion fish | source
  powershell:  marina completion powershell | Out-String | Invoke-Expression

To load completions permanently:

  bash (macOS via Homebrew bash-completion):
    marina completion bash > $(brew --prefix)/etc/bash_completion.d/marina

  bash (Linux):
    marina completion bash > /etc/bash_completion.d/marina

  zsh:
    echo "autoload -U compinit; compinit" >> ~/.zshrc
    marina completion zsh > "${fpath[1]}/_marina"

  fish:
    marina completion fish > ~/.config/fish/completions/marina.fish

  powershell:
    marina completion powershell >> $PROFILE
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
