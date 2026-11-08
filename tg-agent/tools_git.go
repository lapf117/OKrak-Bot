package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// gitReadActions are safe queries available even in read-only mode.
var gitReadActions = []string{"status", "diff", "log", "branches", "show"}

func toolGitContext(ctx context.Context, args map[string]interface{}) (string, error) {
	action, msg := strArg(args, "action"), strArg(args, "message")
	dir := absPathContext(ctx, strArg(args, "path"))
	// Hooks, fsmonitor and external diff/textconv drivers are disabled so that a
	// repository cannot run its own code through "read-only" queries.
	prefix := "git --no-pager --no-optional-locks -c core.fsmonitor=false -c core.hooksPath=/dev/null -C " + shellQuote(dir) + " "
	var command string
	switch action {
	case "status":
		command = prefix + "status --short --branch"
	case "diff":
		command = prefix + "diff --no-ext-diff --no-textconv --stat && " + prefix + "diff --no-ext-diff --no-textconv"
	case "log":
		command = prefix + "log --no-show-signature --oneline --decorate -20"
	case "branches":
		command = prefix + "branch -a"
	case "show":
		command = prefix + "show --no-ext-diff --no-textconv --no-show-signature --stat HEAD"
	case "add":
		// Agent backup/temporary files are never staged.
		command = prefix + "add -A -- . " + shellQuote(":(exclude)*.agentbak.*") + " " + shellQuote(":(exclude)*.agenttmp.*") + " && " + prefix + "status --short"
	case "commit":
		if strings.TrimSpace(msg) == "" {
			return "", fmt.Errorf("commit 必须明确提供 message")
		}
		command = prefix + "commit -m " + shellQuote(msg) + " && " + prefix + "log --no-show-signature --oneline -1"
	default:
		return "", fmt.Errorf("未知 git 操作: %s", action)
	}
	return runShellContext(ctx, command, 90*time.Second)
}
