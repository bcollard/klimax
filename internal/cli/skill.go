package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bcollard/klimax"
	"github.com/spf13/cobra"
)

// claudeSkillDir returns Claude Code's user skills directory for klimax.
func claudeSkillDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "skills", "klimax")
}

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Install the klimax Agent Skill for AI coding tools",
		Long: `Manage the klimax Agent Skill — a portable capability description that
teaches AI coding tools (e.g. Claude Code) how to use klimax to spin up
ephemeral kind clusters for scripts, demos, and e2e tests.`,
	}
	cmd.AddCommand(newSkillInstallCmd(), newSkillPathCmd())
	return cmd
}

func newSkillInstallCmd() *cobra.Command {
	var (
		claude bool
		print  bool
		force  bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the klimax Agent Skill into an AI coding tool",
		Long: `Install the klimax Agent Skill (SKILL.md) into your AI coding tool's
skills directory, so agents know how to drive klimax without you explaining it
each session.

By default it installs into Claude Code's user skills directory:

  ~/.claude/skills/klimax/SKILL.md

Use --print to emit the skill to stdout instead (pipe it anywhere).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if print {
				fmt.Fprint(os.Stdout, klimax.SkillMD)
				return nil
			}
			if !claude {
				return fmt.Errorf("no install target selected (pass --claude, or --print)")
			}
			return installClaudeSkill(force)
		},
	}
	cmd.Flags().BoolVar(&claude, "claude", true, "Install into Claude Code's user skills directory (~/.claude/skills/klimax)")
	cmd.Flags().BoolVar(&print, "print", false, "Write the skill to stdout instead of installing")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Overwrite an existing installed skill")
	return cmd
}

func newSkillPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print where the klimax Agent Skill is installed for Claude Code",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(filepath.Join(claudeSkillDir(), "SKILL.md"))
			return nil
		},
	}
}

func installClaudeSkill(force bool) error {
	dir := claudeSkillDir()
	dest := filepath.Join(dir, "SKILL.md")

	if _, err := os.Stat(dest); err == nil && !force {
		fmt.Printf("Skill already installed at %s (use --force to overwrite)\n", dest)
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating skill dir: %w", err)
	}
	if err := os.WriteFile(dest, []byte(klimax.SkillMD), 0o644); err != nil {
		return fmt.Errorf("writing skill: %w", err)
	}

	fmt.Printf("Installed klimax Agent Skill → %s\n", dest)
	fmt.Println("Start a new AI coding session to pick it up.")
	return nil
}
