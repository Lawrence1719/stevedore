package build

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// gitCmd builds an *exec.Cmd for running git in the given directory.
// Always uses a fixed binary path and a slice of arguments — never sh -c.
func gitCmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd
}

// runGit runs a git subcommand using a fixed binary and a slice of arguments
// (no shell interpolation, no fmt.Sprintf into command strings).
func runGit(ctx context.Context, w io.Writer, dir string, args ...string) error {
	cmd := gitCmd(ctx, dir, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
