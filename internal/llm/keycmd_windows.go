//go:build windows

package llm

import (
	"context"
	"os/exec"
)

func newKeyCmd(ctx context.Context, cmd string) *exec.Cmd {
	return exec.CommandContext(ctx, "cmd", "/C", cmd)
}
