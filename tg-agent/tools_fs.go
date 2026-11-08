package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxEditableFile = 2 << 20

var fileWriteGate = make(chan struct{}, 1)

func lockToolFiles(ctx context.Context) (func(), error) {
	select {
	case fileWriteGate <- struct{}{}:
		return func() { <-fileWriteGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Tool state belongs to a task; sub-agents share it, other chats never do.
type toolTaskState struct {
	mu      sync.Mutex
	undo    []undoEntry
	backups []string        // every backup created by this task, for cleanup
	sent    map[string]bool // files already delivered with send_file
}

type toolStateKey struct{}

type undoEntry struct {
	Path, Backup string
	Existed      bool
}

// withTodoContext attaches task tool state and a plan when absent.
func withTodoContext(ctx context.Context) context.Context {
	if _, ok := ctx.Value(toolStateKey{}).(*toolTaskState); !ok {
		ctx = context.WithValue(ctx, toolStateKey{}, &toolTaskState{})
	}
	if _, ok := ctx.Value(planStateKey{}).(*planState); !ok {
		ctx = withPlanContext(ctx)
	}
	return ctx
}

func taskToolState(ctx context.Context) *toolTaskState {
	if state, ok := ctx.Value(toolStateKey{}).(*toolTaskState); ok {
		return state
	}
	return &toolTaskState{}
}

func markSent(ctx context.Context, path string) {
	state := taskToolState(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sent == nil {
		state.sent = map[string]bool{}
	}
	state.sent[path] = true
}

func sentFiles(ctx context.Context) map[string]bool {
	state := taskToolState(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	out := map[string]bool{}
	for k := range state.sent {
		out[k] = true
	}
	return out
}

func absPath(p string) string { return absPathContext(context.Background(), p) }

func absPathContext(ctx context.Context, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(executionConfig(ctx).Workspace, p)
}

// ---------------- backups & undo ----------------

func snapshotContext(ctx context.Context, path string) error {
	full := absPathContext(ctx, path)
	bak := full + ".agentbak." + strconv.FormatInt(time.Now().UnixNano(), 36)
	q := shellQuote(full)
	script := "if [ -L " + q + " ]; then echo '拒绝编辑符号链接' >&2; exit 1; elif [ -f " + q + " ]; then size=$(wc -c < " + q + "); [ \"$size\" -le 33554432 ] || { echo '文件超过 32 MiB，拒绝自动备份和覆盖' >&2; exit 1; }; cp -p -- " + q + " " + shellQuote(bak) + " && echo existed; elif [ -e " + q + " ]; then echo '路径不是普通文件' >&2; exit 1; else echo absent; fi"
	out, err := runShellContext(ctx, script, 30*time.Second)
	if err != nil {
		return fmt.Errorf("备份失败: %w", err)
	}
	existed := strings.TrimSpace(out) == "existed"
	state := taskToolState(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.undo = append(state.undo, undoEntry{Path: full, Backup: bak, Existed: existed})
	if existed {
		state.backups = append(state.backups, bak)
	}
	if len(state.undo) > 50 {
		state.undo = state.undo[len(state.undo)-50:]
	}
	return nil
}

// cleanupTaskBackups removes the .agentbak files this task created.
func cleanupTaskBackups(ctx context.Context) {
	state := taskToolState(ctx)
	state.mu.Lock()
	backups := state.backups
	state.backups, state.undo = nil, nil
	state.mu.Unlock()
	for len(backups) > 0 {
		n := min(len(backups), 100)
		quoted := make([]string, n)
		for i, b := range backups[:n] {
			quoted[i] = shellQuote(b)
		}
		if _, err := runShellContext(ctx, "rm -f -- "+strings.Join(quoted, " "), 30*time.Second); err != nil {
			logf("清理备份失败: %v", err)
			return
		}
		backups = backups[n:]
	}
}

func toolUndoContext(ctx context.Context, args map[string]interface{}) (string, error) {
	unlock, lockErr := lockToolFiles(ctx)
	if lockErr != nil {
		return "", lockErr
	}
	defer unlock()
	n := intArg(args, "count", 1, 1, 5)
	state := taskToolState(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	var restored []string
	for i := 0; i < n && len(state.undo) > 0; i++ {
		e := state.undo[len(state.undo)-1]
		script := "rm -f -- " + shellQuote(e.Path)
		if e.Existed {
			script = "cp -p -- " + shellQuote(e.Backup) + " " + shellQuote(e.Path)
		}
		if _, err := runShellContext(ctx, script, 30*time.Second); err != nil {
			return strings.Join(restored, "\n"), err
		}
		state.undo = state.undo[:len(state.undo)-1]
		restored = append(restored, e.Path)
	}
	if len(restored) == 0 {
		return "没有本任务可撤销的改动。", nil
	}
	return "已恢复：\n" + strings.Join(restored, "\n"), nil
}

// ---------------- reading ----------------

func toolReadFileContext(ctx context.Context, args map[string]interface{}) (string, error) {
	p := strArg(args, "path")
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	full := absPathContext(ctx, p)
	offset := intArg(args, "offset", 1, 1, 1000000000)
	limit := intArg(args, "limit", 200, 1, 2000)
	if out, err := runShellContext(ctx, "test -d "+shellQuote(full)+" && echo DIR", 10*time.Second); err == nil && strings.Contains(out, "DIR") {
		return runShellContext(ctx, "ls -la -- "+shellQuote(full)+" | head -200", 30*time.Second)
	}
	out, err := runShellContext(ctx, fmt.Sprintf("sed -n '%d,%dp' -- %s", offset, offset+limit-1, shellQuote(full)), 30*time.Second)
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(out) == "" {
		return "（文件为空或超出范围）", nil
	}
	if total, e := runShellContext(ctx, "wc -l < "+shellQuote(full), 15*time.Second); e == nil {
		out += fmt.Sprintf("\n\n[第 %d 行起，共 %s 行]", offset, strings.TrimSpace(total))
	}
	return out, nil
}

func toolGlobContext(ctx context.Context, args map[string]interface{}) (string, error) {
	pattern := strArg(args, "pattern")
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	full := absPathContext(ctx, strArg(args, "path"))
	command := "cd -- " + shellQuote(full) + " || exit $?; if command -v rg >/dev/null 2>&1; then rg --files --hidden --glob '!.git/**' --glob '!node_modules/**' --glob " + shellQuote(pattern) + "; else find . -type d \\( -name .git -o -name node_modules \\) -prune -o -type f -name " + shellQuote(pattern) + " -print; fi; rc=$?; if [ \"$rc\" -eq 1 ]; then exit 0; else exit \"$rc\"; fi"
	out, err := runShellContext(ctx, command, 60*time.Second)
	if err == nil && strings.TrimSpace(out) == "" {
		return "（没有匹配文件）", nil
	}
	return out, err
}

func toolListDirContext(ctx context.Context, args map[string]interface{}) (string, error) {
	return runShellContext(ctx, "ls -la -- "+shellQuote(absPathContext(ctx, strArg(args, "path"))), 30*time.Second)
}

func toolRepoMapContext(ctx context.Context, args map[string]interface{}) (string, error) {
	path := strArg(args, "path")
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	depth := intArg(args, "depth", 3, 1, 12)
	script := fmt.Sprintf(`cd %s && {
  echo "== 目录结构 ==";
  find . -maxdepth %d -type d -not -path '*/.git*' -not -path '*/node_modules*' | sort | head -60;
  echo;
  echo "== 文件总数 ==";
  find . -type f -not -path '*/.git/*' -not -path '*/node_modules/*' | wc -l;
  echo;
  echo "== 主要文件 ==";
  find . -maxdepth %d -type f -not -path '*/.git/*' -not -path '*/node_modules/*' -size +1k | sort | head -80;
  echo;
  echo "== 语言分布 ==";
  find . -type f -not -path '*/.git/*' -not -path '*/node_modules/*' -name '*.*' | sed 's/.*\.//' | sort | uniq -c | sort -rn | head -12;
} 2>/dev/null`, shellQuote(absPathContext(ctx, path)), depth, depth)
	return runShellContext(ctx, script, 60*time.Second)
}

func toolGrepContext(ctx context.Context, args map[string]interface{}) (string, error) {
	pat, include := strArg(args, "pattern"), strArg(args, "include")
	if strings.TrimSpace(pat) == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	full := absPathContext(ctx, strArg(args, "path"))
	glob, fallback := "", ""
	if include != "" {
		glob = " --glob " + shellQuote(include)
		fallback = " --include=" + shellQuote(include)
	}
	command := "if command -v rg >/dev/null 2>&1; then rg -n --no-heading --color never --no-messages" + glob + " -e " + shellQuote(pat) + " -- " + shellQuote(full) + "; else grep -rnI --color=never" + fallback + " -e " + shellQuote(pat) + " -- " + shellQuote(full) + "; fi; rc=$?; if [ \"$rc\" -eq 1 ]; then exit 0; else exit \"$rc\"; fi"
	out, err := runShellContext(ctx, command, 60*time.Second)
	if err == nil && strings.TrimSpace(out) == "" {
		return "（没有匹配）", nil
	}
	return out, err
}

// ---------------- writing ----------------

func atomicWriteContext(ctx context.Context, full, content string) (string, error) {
	if len(content) > maxEditableFile {
		return "", fmt.Errorf("单个文件上限 2 MiB，请使用 bash 分块处理")
	}
	// Payload travels over stdin rather than command arguments (ARG_MAX).
	encoded := []byte(base64.StdEncoding.EncodeToString([]byte(content)))
	q := shellQuote(full)
	script := "set -e; mkdir -p -- " + shellQuote(filepath.Dir(full)) + "; tmp=$(mktemp " + shellQuote(full+".agenttmp.XXXXXX") + "); trap 'rm -f -- \"$tmp\"' EXIT; if [ -f " + q + " ]; then chmod --reference=" + q + " \"$tmp\"; else chmod 600 \"$tmp\"; fi; base64 -d > \"$tmp\"; mv -f -- \"$tmp\" " + q + "; wc -c < " + q
	return runShellInputContext(ctx, script, 60*time.Second, encoded, outputLimit(ctx), false)
}

func toolWriteFileContext(ctx context.Context, args map[string]interface{}) (string, error) {
	unlock, lockErr := lockToolFiles(ctx)
	if lockErr != nil {
		return "", lockErr
	}
	defer unlock()
	p := strArg(args, "path")
	content, ok := args["content"].(string)
	if strings.TrimSpace(p) == "" || !ok {
		return "", fmt.Errorf("path 和 content 字符串不能为空")
	}
	full := absPathContext(ctx, p)
	if err := snapshotContext(ctx, p); err != nil {
		return "", err
	}
	out, err := atomicWriteContext(ctx, full, content)
	return "已写入 " + full + "（" + strings.TrimSpace(out) + " 字节）", err
}

func toolEditFileContext(ctx context.Context, args map[string]interface{}) (string, error) {
	unlock, lockErr := lockToolFiles(ctx)
	if lockErr != nil {
		return "", lockErr
	}
	defer unlock()
	p, oldS := strArg(args, "path"), strArg(args, "old_string")
	newS, ok := args["new_string"].(string)
	if strings.TrimSpace(p) == "" || oldS == "" || !ok {
		return "", fmt.Errorf("path、old_string 和 new_string 字符串必填")
	}
	full := absPathContext(ctx, p)
	// Never edit a partial read: refuse oversized/binary files before snapshot.
	content, err := runShellInputContext(ctx, "cat -- "+shellQuote(full), 30*time.Second, nil, maxEditableFile, true)
	if err != nil {
		return "", fmt.Errorf("无法完整读取文件，未修改: %w", err)
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return "", fmt.Errorf("拒绝精确编辑二进制文件")
	}
	replaceAll, _ := args["replace_all"].(bool)
	n := strings.Count(content, oldS)
	switch {
	case n == 0:
		return "", fmt.Errorf("文件里找不到这段文本（注意空格、缩进与换行需完全一致）")
	case n > 1 && !replaceAll:
		return "", fmt.Errorf("文本出现 %d 次，请提供唯一上下文，或设置 replace_all=true", n)
	}
	if err := snapshotContext(ctx, p); err != nil {
		return "", err
	}
	updated := strings.Replace(content, oldS, newS, 1)
	if replaceAll {
		updated = strings.ReplaceAll(content, oldS, newS)
	}
	out, err := atomicWriteContext(ctx, full, updated)
	return fmt.Sprintf("已修改 %s（替换 %d 处，%s 字节）", full, map[bool]int{true: n, false: 1}[replaceAll], strings.TrimSpace(out)), err
}
