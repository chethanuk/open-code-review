//go:build !windows

package llm

import (
	"context"
	"os/exec"
)

// newKeyCmd builds the OS-specific shell invocation (sh -c on Unix) that runs a
// credential command under ctx, so its timeout and cancellation are honored.
func newKeyCmd(ctx context.Context, cmd string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}
