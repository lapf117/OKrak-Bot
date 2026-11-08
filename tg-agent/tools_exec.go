package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxToolOutput = 12000

var errOutputTruncated = errors.New("输出超出上限")

// boundedOutput collects stdout/stderr up to a limit and drops the rest.
type boundedOutput struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if remain := b.limit - len(b.data); remain < len(p) {
		b.truncated = true
		p = p[:max(remain, 0)]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *boundedOutput) result() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data), b.truncated
}

func outputLimit(ctx context.Context) int {
	n := executionConfig(ctx).MaxToolOutput
	if n <= 0 {
		return maxToolOutput
	}
	return min(n, 1<<20)
}

// commandLimit caps a requested timeout at the configured command timeout.
func commandLimit(ctx context.Context, requested time.Duration) time.Duration {
	limit := time.Duration(maxPositive(executionConfig(ctx).CommandTimeoutSeconds, 120)) * time.Second
	if requested <= 0 || requested > limit {
		return limit
	}
	return requested
}

func runShell(command string, timeout time.Duration) (string, error) {
	return runShellContext(context.Background(), command, timeout)
}

func runShellContext(ctx context.Context, command string, timeout time.Duration) (string, error) {
	return runShellInputContext(ctx, command, timeout, nil, outputLimit(ctx), false)
}

// runShellInputContext runs bash locally or over SSH in the task workspace.
func runShellInputContext(ctx context.Context, command string, timeout time.Duration, input []byte, limit int, strict bool) (string, error) {
	c := executionConfig(ctx)
	var argv []string
	if c.Backend == "ssh" {
		remote := "cd -- " + shellQuote(c.Workspace) + " && exec bash -c " + shellQuote(command)
		argv = []string{"ssh", "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "-o", "ConnectionAttempts=1", "-p", strconv.Itoa(maxPositive(c.SSH.Port, 22))}
		if c.SSH.KeyPath != "" {
			argv = append(argv, "-i", c.SSH.KeyPath)
		}
		argv = append(argv, "--", c.SSH.User+"@"+c.SSH.Host, remote)
	} else {
		argv = []string{"bash", "-c", command}
	}
	return runCmdContext(ctx, argv, commandLimit(ctx, timeout), input, limit, strict)
}

func runCmd(args []string, timeout time.Duration) (string, error) {
	return runCmdContext(context.Background(), args, timeout, nil, maxToolOutput, false)
}

// runCmdContext kills the whole process group on timeout or cancellation.
// In strict mode truncated output is an error instead of a marker.
func runCmdContext(ctx context.Context, args []string, timeout time.Duration, input []byte, limit int, strict bool) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("空命令")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	if executionConfig(ctx).Backend != "ssh" {
		c.Dir = executionConfig(ctx).Workspace
	}
	c.Stdin = bytes.NewReader(input)
	out := &boundedOutput{limit: limit}
	c.Stdout, c.Stderr = out, out
	configureProcess(c)
	if err := c.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		killProcessTree(c)
		err = <-done
	}
	result, truncated := out.result()
	if !strict {
		result = strings.ToValidUTF8(result, "�")
	}
	if truncated {
		if strict {
			return "", errOutputTruncated
		}
		result += "\n…（输出过长已截断，请缩小查询或分页）"
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		return result, fmt.Errorf("命令失败: %w", err)
	}
	return result, nil
}

// intArg reads a numeric argument, clamping out-of-range values instead of
// silently replacing them with the default.
func intArg(args map[string]interface{}, key string, def, lo, hi int) int {
	v, ok := args[key].(float64)
	if !ok {
		return def
	}
	switch {
	case v < float64(lo):
		return lo
	case v > float64(hi):
		return hi
	}
	return int(v)
}

func strArg(args map[string]interface{}, key string) string {
	s, _ := args[key].(string)
	return s
}

func toolBashContext(ctx context.Context, args map[string]interface{}) (string, error) {
	cmd := strArg(args, "command")
	if strings.TrimSpace(cmd) == "" {
		return "", fmt.Errorf("command 不能为空")
	}
	timeout := intArg(args, "timeout", 120, 1, 86400)
	return runShellContext(ctx, cmd, time.Duration(timeout)*time.Second)
}
