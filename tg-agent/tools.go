package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxToolOutput = 12000
const maxEditableFile = 2 << 20

var errOutputTruncated = errors.New("输出超出上限")
var fileWriteGate = make(chan struct{}, 1)

func lockToolFiles(ctx context.Context) (func(), error) {
	select {
	case fileWriteGate <- struct{}{}:
		return func() { <-fileWriteGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

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
	remain := b.limit - len(b.data)
	if remain < len(p) {
		b.truncated = true
		p = p[:remain]
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
	if n > 1<<20 {
		return 1 << 20
	}
	return n
}
func commandLimit(ctx context.Context, requested time.Duration) time.Duration {
	n := executionConfig(ctx).CommandTimeoutSeconds
	if n <= 0 {
		n = 120
	}
	cap := time.Duration(n) * time.Second
	if requested <= 0 || requested > cap {
		return cap
	}
	return requested
}
func runShell(command string, timeout time.Duration) (string, error) {
	return runShellContext(context.Background(), command, timeout)
}
func runShellContext(ctx context.Context, command string, timeout time.Duration) (string, error) {
	return runShellInputContext(ctx, command, timeout, nil, outputLimit(ctx), false)
}
func runShellInputContext(ctx context.Context, command string, timeout time.Duration, input []byte, limit int, strict bool) (string, error) {
	c := executionConfig(ctx)
	var argv []string
	if c.Backend == "ssh" {
		remote := "cd -- " + shellQuote(c.Workspace) + " && exec bash -c " + shellQuote(command)
		port := c.SSH.Port
		if port == 0 {
			port = 22
		}
		argv = []string{"ssh", "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "-o", "ConnectionAttempts=1", "-p", strconv.Itoa(port)}
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
	c.Stdout = out
	c.Stderr = out
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

// Tool state belongs to a task; children share it, other chats never do.
type toolTaskState struct {
	mu    sync.Mutex
	todos []todoItem
	undo  []undoEntry
}
type toolStateKey struct{}

func withTodoContext(ctx context.Context) context.Context {
	if _, ok := ctx.Value(toolStateKey{}).(*toolTaskState); ok {
		return ctx
	}
	return context.WithValue(ctx, toolStateKey{}, &toolTaskState{})
}
func taskToolState(ctx context.Context) *toolTaskState {
	if state, ok := ctx.Value(toolStateKey{}).(*toolTaskState); ok {
		return state
	}
	return &toolTaskState{}
}

type undoEntry struct {
	Path, Backup string
	Existed      bool
}
type todoItem struct {
	ID   int
	Text string
	Done bool
}

func snapshotContext(ctx context.Context, path string) error {
	full := absPathContext(ctx, path)
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	bak := full + ".agentbak." + token
	script := "if [ -L " + shellQuote(full) + " ]; then echo '拒绝编辑符号链接' >&2; exit 1; elif [ -f " + shellQuote(full) + " ]; then size=$(wc -c < " + shellQuote(full) + "); [ \"$size\" -le 33554432 ] || { echo '文件超过 32 MiB，拒绝自动备份和覆盖' >&2; exit 1; }; cp -p -- " + shellQuote(full) + " " + shellQuote(bak) + " && echo existed; elif [ -e " + shellQuote(full) + " ]; then echo '路径不是普通文件' >&2; exit 1; else echo absent; fi"
	out, err := runShellContext(ctx, script, 30*time.Second)
	if err != nil {
		return fmt.Errorf("备份失败: %w", err)
	}
	state := taskToolState(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.undo = append(state.undo, undoEntry{Path: full, Backup: bak, Existed: strings.TrimSpace(out) == "existed"})
	if len(state.undo) > 50 {
		state.undo = state.undo[len(state.undo)-50:]
	}
	return nil
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
		var script string
		if e.Existed {
			script = "cp -p -- " + shellQuote(e.Backup) + " " + shellQuote(e.Path)
		} else {
			script = "rm -f -- " + shellQuote(e.Path)
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
func toolTodoContext(ctx context.Context, args map[string]interface{}) (string, error) {
	state := taskToolState(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	action, _ := args["action"].(string)
	text, _ := args["text"].(string)
	switch action {
	case "add":
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("text 不能为空")
		}
		if len(state.todos) >= 100 {
			return "", fmt.Errorf("清单最多 100 项")
		}
		state.todos = append(state.todos, todoItem{ID: len(state.todos) + 1, Text: short(text, 1000)})
	case "done":
		id := intArg(args, "id", 0, 0, 100)
		found := false
		for i := range state.todos {
			if state.todos[i].ID == id {
				state.todos[i].Done = true
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("任务编号不存在")
		}
	case "clear":
		state.todos = nil
	case "list", "":
	default:
		return "", fmt.Errorf("未知清单操作")
	}
	if len(state.todos) == 0 {
		return "（清单为空）", nil
	}
	var b strings.Builder
	for _, t := range state.todos {
		mark := "[ ]"
		if t.Done {
			mark = "[x]"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", mark, t.ID, t.Text)
	}
	return b.String(), nil
}
func todoSnapshot() string { return "" }
func todoSnapshotContext(ctx context.Context) string {
	state := taskToolState(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	var b strings.Builder
	for _, t := range state.todos {
		if !t.Done {
			fmt.Fprintf(&b, "- %s\n", t.Text)
		}
	}
	return b.String()
}
func intArg(args map[string]interface{}, key string, def, min, max int) int {
	n := def
	if v, ok := args[key].(float64); ok {
		if v < float64(min) || v > float64(max) {
			return def
		}
		n = int(v)
	}
	return n
}

// ---------------- 工具实现 ----------------

func toolBashContext(ctx context.Context, args map[string]interface{}) (string, error) {
	cmd, _ := args["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return "", fmt.Errorf("command 不能为空")
	}
	tmo := 120
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		tmo = int(v)
	}
	out, err := runShellContext(ctx, cmd, time.Duration(tmo)*time.Second)
	return out, err
}

func absPath(p string) string { return absPathContext(context.Background(), p) }
func absPathContext(ctx context.Context, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(executionConfig(ctx).Workspace, p)
}

func toolReadFileContext(ctx context.Context, args map[string]interface{}) (string, error) {
	p, _ := args["path"].(string)
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	full := absPathContext(ctx, p)
	offset := intArg(args, "offset", 1, 1, 1000000000)
	limit := intArg(args, "limit", 200, 1, 2000)
	// 目录：列条目
	if out, err := runShellContext(ctx, "test -d "+shellQuote(full)+" && echo DIR", 10*time.Second); err == nil && strings.Contains(out, "DIR") {
		return runShellContext(ctx, "ls -la -- "+shellQuote(full)+" | head -200", 30*time.Second)
	}
	var cmd string
	if offset > 0 {
		cmd = fmt.Sprintf("sed -n '%d,%dp' -- %s", offset, offset+limit-1, shellQuote(full))
	} else {
		cmd = fmt.Sprintf("head -n %d -- %s", limit, shellQuote(full))
	}
	out, err := runShellContext(ctx, cmd, 30*time.Second)
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(out) == "" {
		return "（文件为空或超出范围）", nil
	}
	// 附上总行数提示
	if tot, e := runShellContext(ctx, "wc -l < "+shellQuote(full), 15*time.Second); e == nil {
		out += fmt.Sprintf("\n\n[共 %s 行]", strings.TrimSpace(tot))
	}
	return out, nil
}

func toolGlobContext(ctx context.Context, args map[string]interface{}) (string, error) {
	pattern, _ := args["pattern"].(string)
	path, _ := args["path"].(string)
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	full := absPathContext(ctx, path)
	command := "cd -- " + shellQuote(full) + " || exit $?; if command -v rg >/dev/null 2>&1; then rg --files --hidden --glob '!.git/**' --glob '!node_modules/**' --glob " + shellQuote(pattern) + "; else find . -type d \\( -name .git -o -name node_modules \\) -prune -o -type f -name " + shellQuote(pattern) + " -print; fi; rc=$?; if [ \"$rc\" -eq 1 ]; then exit 0; else exit \"$rc\"; fi"
	out, err := runShellContext(ctx, command, 60*time.Second)
	if err == nil && strings.TrimSpace(out) == "" {
		return "（没有匹配文件）", nil
	}
	return out, err
}

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
	p, _ := args["path"].(string)
	content, ok := args["content"].(string)
	if strings.TrimSpace(p) == "" || !ok {
		return "", fmt.Errorf("path 和 content 字符串不能为空")
	}
	full := absPathContext(ctx, p)
	if err := snapshotContext(ctx, p); err != nil {
		return "", err
	}
	out, err := atomicWriteContext(ctx, full, content)
	return "已写入 " + full + "\n" + strings.TrimSpace(out), err
}
func toolEditFileContext(ctx context.Context, args map[string]interface{}) (string, error) {
	unlock, lockErr := lockToolFiles(ctx)
	if lockErr != nil {
		return "", lockErr
	}
	defer unlock()
	p, _ := args["path"].(string)
	oldS, _ := args["old_string"].(string)
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
	n := strings.Count(content, oldS)
	if n == 0 {
		return "", fmt.Errorf("文件里找不到这段文本")
	}
	if n > 1 {
		return "", fmt.Errorf("文本出现 %d 次，请提供唯一上下文", n)
	}
	if err := snapshotContext(ctx, p); err != nil {
		return "", err
	}
	updated := strings.Replace(content, oldS, newS, 1)
	out, err := atomicWriteContext(ctx, full, updated)
	return "已修改 " + full + "\n" + strings.TrimSpace(out), err
}
func toolGitContext(ctx context.Context, args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	msg, _ := args["message"].(string)
	path, _ := args["path"].(string)
	dir := absPathContext(ctx, path)
	prefix := "git --no-pager --no-optional-locks -c core.fsmonitor=false -c core.hooksPath=/dev/null -C " + shellQuote(dir) + " "
	var command string
	switch action {
	case "status":
		command = prefix + "status --short --branch"
	case "diff":
		command = prefix + "diff --no-ext-diff --no-textconv --stat && " + prefix + "diff --no-ext-diff --no-textconv"
	case "log":
		command = prefix + "log --no-show-signature --oneline -15"
	case "branches":
		command = prefix + "branch -a"
	case "add":
		command = prefix + "add -A && " + prefix + "status --short"
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

func toolRepoMapContext(ctx context.Context, args map[string]interface{}) (string, error) {
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	depth := intArg(args, "depth", 3, 1, 12)
	full := absPathContext(ctx, path)
	script := fmt.Sprintf(`cd %s && {
  echo "== 目录结构 ==";
  find . -maxdepth %d -type d -not -path '*/.git*' -not -path '*/node_modules*' | head -60 | sort;
  echo;
  echo "== 文件总数 ==";
  find . -type f -not -path '*/.git/*' -not -path '*/node_modules/*' | wc -l;
  echo;
  echo "== 主要文件 ==";
  find . -maxdepth %d -type f -not -path '*/.git/*' -not -path '*/node_modules/*' -size +1k | head -80 | sort;
  echo;
  echo "== 语言分布 ==";
  find . -type f -not -path '*/.git/*' -not -path '*/node_modules/*' | sed 's/.*\.//' | sort | uniq -c | sort -rn | head -12;
} 2>/dev/null`, shellQuote(full), depth, depth)
	return runShellContext(ctx, script, 60*time.Second)
}

func toolGrepContext(ctx context.Context, args map[string]interface{}) (string, error) {
	pat, _ := args["pattern"].(string)
	path, _ := args["path"].(string)
	include, _ := args["include"].(string)
	if strings.TrimSpace(pat) == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	full := absPathContext(ctx, path)
	glob := ""
	if include != "" {
		glob = " --glob " + shellQuote(include)
	}
	fallback := ""
	if include != "" {
		fallback = " --include=" + shellQuote(include)
	}
	command := "if command -v rg >/dev/null 2>&1; then rg -n --no-heading --color never --no-messages" + glob + " -e " + shellQuote(pat) + " -- " + shellQuote(full) + "; else grep -rnI --color=never" + fallback + " -e " + shellQuote(pat) + " -- " + shellQuote(full) + "; fi; rc=$?; if [ \"$rc\" -eq 1 ]; then exit 0; else exit \"$rc\"; fi"
	out, err := runShellContext(ctx, command, 60*time.Second)
	if err == nil && strings.TrimSpace(out) == "" {
		return "（没有匹配）", nil
	}
	return out, err
}
func toolListDirContext(ctx context.Context, args map[string]interface{}) (string, error) {
	p, _ := args["path"].(string)
	return runShellContext(ctx, "ls -la -- "+shellQuote(absPathContext(ctx, p)), 30*time.Second)
}

// netClient 联网用的客户端，支持配置代理
func netClient() *http.Client { return netClientContext(context.Background()) }
func netClientContext(ctx context.Context) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy := strings.TrimSpace(executionConfig(ctx).Proxy); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: 45 * time.Second, Transport: transport}
}

func toolNetCheckContext(ctx context.Context, args map[string]interface{}) (string, error) {
	targets := executionConfig(ctx).NetCheckURLs
	if len(targets) == 0 {
		targets = []string{
			"https://api.deepseek.com",
			"https://www.baidu.com",
			"https://www.google.com",
			"https://api.telegram.org",
			"https://github.com",
			"https://pypi.org",
		}
	}
	var b strings.Builder
	b.WriteString("网络自检\n")
	b.WriteString(fmt.Sprintf("代理: %s\n\n", orNone(executionConfig(ctx).Proxy)))
	cl := netClientContext(ctx)
	defer cl.CloseIdleConnections()
	for _, t := range targets {
		start := time.Now()
		req, reqErr := http.NewRequestWithContext(ctx, "GET", t, nil)
		if reqErr != nil {
			return b.String(), reqErr
		}
		resp, err := cl.Do(req)
		if err != nil {
			b.WriteString(fmt.Sprintf("失败  %-32s %s\n", t, short(err.Error(), 50)))
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		b.WriteString(fmt.Sprintf("%d     %-32s %.0fms\n", resp.StatusCode, t, float64(time.Since(start).Milliseconds())))
	}
	// DNS
	if out, err := runShellContext(ctx, "getent hosts api.deepseek.com | head -1", 10*time.Second); err == nil {
		b.WriteString("\nDNS: " + strings.TrimSpace(out))
	}
	return b.String(), nil
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（直连）"
	}
	if u, err := url.Parse(s); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return s
}

var tagRe = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`)
var htmlRe = regexp.MustCompile(`<[^>]+>`)
var spaceRe = regexp.MustCompile(`\n{3,}`)

func toolWebFetchContext(ctx context.Context, args map[string]interface{}) (string, error) {
	u, _ := args["url"].(string)
	parsed, parseErr := url.Parse(u)
	if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("只支持 http/https")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; TG-Agent/2.0)")
	cl := netClientContext(ctx)
	defer cl.CloseIdleConnections()
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (3<<20)+1))
	if readErr != nil {
		return "", readErr
	}
	truncated := len(body) > 3<<20
	if truncated {
		body = body[:3<<20]
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, short(string(body), 200))
	}
	text := string(body)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		text = tagRe.ReplaceAllString(text, " ")
		text = htmlRe.ReplaceAllString(text, " ")
	}
	text = html.UnescapeString(text)
	text = spaceRe.ReplaceAllString(text, "\n\n")
	text = strings.TrimSpace(text)
	if truncated {
		text += "\n…（网页内容已截断）"
	}
	return fmt.Sprintf("HTTP %d\n\n%s", resp.StatusCode, text), nil
}

func toolWebSearchContext(ctx context.Context, args map[string]interface{}) (string, error) {
	q, _ := args["query"].(string)
	if strings.TrimSpace(q) == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(q)
	req, reqErr := http.NewRequestWithContext(ctx, "GET", u, nil)
	if reqErr != nil {
		return "", reqErr
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; TG-Agent/2.0)")
	cl := netClientContext(ctx)
	defer cl.CloseIdleConnections()
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("搜索 HTTP %d", resp.StatusCode)
	}
	html := string(body)

	resRe := regexp.MustCompile(`(?s)class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	matches := resRe.FindAllStringSubmatch(html, 8)
	var b strings.Builder
	for i, m := range matches {
		link := m[1]
		title := htmlRe.ReplaceAllString(m[2], "")
		title = strings.TrimSpace(title)
		b.WriteString(fmt.Sprintf("%d. %s\n   %s\n", i+1, title, link))
	}
	if b.Len() == 0 {
		b.WriteString(short(htmlRe.ReplaceAllString(html, " "), 1500))
	}
	return b.String(), nil
}

// ---------------- 工具注册 ----------------

type ToolDef struct {
	Name   string
	Desc   string
	Params map[string]interface{}
	Run    func(context.Context, map[string]interface{}) (string, error)
	Read   bool // 只读工具，朋友可用
}

func obj(props map[string]interface{}, required ...string) map[string]interface{} {
	m := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func pStr(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func pNum(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "number", "description": desc}
}

func allTools() []ToolDef {
	return []ToolDef{
		{Name: "bash", Desc: "在工作区执行 shell 命令，返回 stdout/stderr。", Read: false,
			Params: obj(map[string]interface{}{
				"command": pStr("要执行的命令"),
				"timeout": pNum("超时秒数，默认 120"),
			}, "command"), Run: toolBashContext},
		{Name: "read_file", Desc: "读取文件内容。大文件用 offset/limit 分页，单次最多 2000 行。传目录则列出条目。", Read: true,
			Params: obj(map[string]interface{}{
				"path":   pStr("文件或目录路径"),
				"offset": pNum("从第几行开始（1 起）"),
				"limit":  pNum("读多少行，默认 200"),
			}, "path"), Run: toolReadFileContext},
		{Name: "glob", Desc: "按文件名模式查找文件，如 *.go、*.json。找文件比 grep 快。", Read: true,
			Params: obj(map[string]interface{}{
				"pattern": pStr("文件名模式，如 *.py"),
				"path":    pStr("搜索目录，默认 ."),
			}, "pattern"), Run: toolGlobContext},
		{Name: "write_file", Desc: "写入文件（覆盖），自动建目录。", Read: false,
			Params: obj(map[string]interface{}{
				"path": pStr("文件路径"), "content": pStr("内容"),
			}, "path", "content"), Run: toolWriteFileContext},
		{Name: "repo_map", Desc: "生成项目结构地图：目录树、文件数、主要文件、语言分布。接手陌生项目先调这个。", Read: true,
			Params: obj(map[string]interface{}{
				"path":  pStr("项目根目录，默认 ."),
				"depth": pNum("目录深度，默认 3"),
			}), Run: toolRepoMapContext},
		{Name: "git", Desc: "git 操作：status / diff / log / add / commit / branches。改代码后用它看改动、提交。", Read: false,
			Params: obj(map[string]interface{}{
				"action":  pStr("status / diff / log / add / commit / branches"),
				"message": pStr("commit 时的提交信息"),
				"path":    pStr("仓库目录，默认 ."),
			}, "action"), Run: toolGitContext},
		{Name: "undo", Desc: "撤销最近的文件改动（write_file / edit_file 会自动留备份）。", Read: false,
			Params: obj(map[string]interface{}{
				"count": pNum("撤销几步，默认 1，最多 5"),
			}), Run: toolUndoContext},
		{Name: "todo", Desc: "维护多步任务清单：add 添加、done 勾选、list 查看、clear 清空。复杂任务先列清单。", Read: true,
			Params: obj(map[string]interface{}{
				"action": pStr("add / done / list / clear"),
				"text":   pStr("任务描述（add 时）"),
				"id":     pNum("任务编号（done 时）"),
			}, "action"), Run: toolTodoContext},
		{Name: "list_dir", Desc: "列出目录内容。", Read: true,
			Params: obj(map[string]interface{}{"path": pStr("目录路径")}, "path"), Run: toolListDirContext},
		{Name: "edit_file", Desc: "精确替换文件里的一段文本。old_string 必须唯一匹配。改代码优先用这个，不要整文件重写。", Read: false,
			Params: obj(map[string]interface{}{
				"path":       pStr("文件路径"),
				"old_string": pStr("要被替换的原文本，必须唯一"),
				"new_string": pStr("替换成的新文本"),
			}, "path", "old_string", "new_string"), Run: toolEditFileContext},
		{Name: "grep", Desc: "在目录里递归搜索文本内容。", Read: true,
			Params: obj(map[string]interface{}{
				"pattern": pStr("正则或普通字符串"),
				"path":    pStr("搜索目录，默认 ."),
				"include": pStr("只搜某类文件，如 *.go"),
			}, "pattern"), Run: toolGrepContext},
		{Name: "web_fetch", Desc: "抓取网页并转成纯文本。", Read: true,
			Params: obj(map[string]interface{}{"url": pStr("URL")}, "url"), Run: toolWebFetchContext},
		{Name: "net_check", Desc: "检测网络连通性、DNS 和代理是否可用。联网出问题时先跑这个。", Read: true,
			Params: obj(map[string]interface{}{}), Run: toolNetCheckContext},
		{Name: "web_search", Desc: "联网搜索，返回标题和链接。", Read: true,
			Params: obj(map[string]interface{}{"query": pStr("搜索词")}, "query"), Run: toolWebSearchContext},
		{Name: "list_skills", Desc: "列出可加载的技能。", Read: true,
			Params: obj(map[string]interface{}{}), Run: toolListSkillsContext},
		{Name: "load_skill", Desc: "加载指定技能的完整说明。", Read: true,
			Params: obj(map[string]interface{}{"name": pStr("技能名")}, "name"), Run: toolLoadSkillContext},
		{Name: "discover_skills", Desc: "搜索管理员配置的可信技能目录，返回可安装技能与校验信息。", Read: true,
			Params: obj(map[string]interface{}{"query": pStr("技能关键词，可空")}), Run: toolDiscoverSkillsContext},
		{Name: "install_skill", Desc: "从管理员配置的可信来源安装文本技能；需通过执行权限与校验。", Read: false,
			Params: obj(map[string]interface{}{"name": pStr("可信目录中的技能名")}, "name"), Run: toolInstallSkillContext},
		{Name: "spawn_agent", Desc: "派出一个子智能体独立完成子任务，返回它的最终结论。适合拆分复杂工作。", Read: false,
			Params: obj(map[string]interface{}{
				"task": pStr("交给子智能体的一句话任务"),
			}, "task"), Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				task, _ := args["task"].(string)
				return spawnAgentContext(ctx, task)
			}},
	}
}

func schemasForPolicy(readOnly, allowInstall bool) []ToolSchema {
	var out []ToolSchema
	for _, t := range allTools() {
		if t.Name == "install_skill" && !allowInstall {
			continue
		}
		if readOnly && !t.Read && t.Name != "git" {
			continue
		}
		if readOnly && t.Name == "git" {
			t.Desc = "只读 Git 查询：status / diff / log / branches。"
			props := t.Params["properties"].(map[string]interface{})
			props["action"] = map[string]interface{}{"type": "string", "enum": []string{"status", "diff", "log", "branches"}, "description": "只读查询操作"}
		}
		var schema ToolSchema
		schema.Type = "function"
		schema.Function.Name = t.Name
		schema.Function.Description = t.Desc
		schema.Function.Parameters = t.Params
		out = append(out, schema)
	}
	return out
}
func toolSchemas(forFriend bool) []ToolSchema {
	return schemasForPolicy(forFriend, !forFriend)
}
func toolSchemasContext(ctx context.Context, uid int64) []ToolSchema {
	c := executionConfig(ctx)
	admin := isAdmin(uid)
	readOnly := c.ApprovalMode == "read-only" || (!admin && c.FriendTools != "all")
	return schemasForPolicy(readOnly, admin)
}
func toolSchemasFor(uid int64) []ToolSchema {
	return toolSchemasContext(context.Background(), uid)
}
func toolReadOnly(name, argsJSON string) bool {
	if name == "todo" {
		return true
	}
	if name == "git" {
		var args map[string]interface{}
		if json.Unmarshal([]byte(argsJSON), &args) != nil {
			return false
		}
		switch args["action"] {
		case "status", "diff", "log", "branches":
			return true
		}
		return false
	}
	for _, t := range allTools() {
		if t.Name == name {
			return t.Read
		}
	}
	return false
}
func runTool(name, argsJSON string) (string, error) {
	return runToolContext(context.Background(), name, argsJSON)
}
func runToolContext(ctx context.Context, name, argsJSON string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(argsJSON) > 4<<20 {
		return "", fmt.Errorf("工具参数过大")
	}
	var args map[string]interface{}
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("参数不是 JSON 对象: %w", err)
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	for _, t := range allTools() {
		if t.Name != name {
			continue
		}
		if t.Run == nil {
			return "", fmt.Errorf("工具未绑定")
		}
		if name == "install_skill" && !isAdmin(executionUID(ctx)) {
			return "", fmt.Errorf("只有管理员可以安装全局技能")
		}
		if err := authorizeTool(ctx, name, argsJSON); err != nil {
			return "", err
		}
		if err := validateToolPathContext(ctx, name, args); err != nil {
			return "", err
		}
		out, err := t.Run(ctx, args)
		limit := outputLimit(ctx)
		if len(out) > limit {
			out = strings.ToValidUTF8(out[:limit], "�") + "\n…（输出过长已截断）"
		}
		return out, err
	}
	return "", fmt.Errorf("没有这个工具: %s", name)
}

// This is an application permission boundary for friends, not an OS sandbox.
func validateToolPathContext(ctx context.Context, name string, args map[string]interface{}) error {
	uid := executionUID(ctx)
	if isAdmin(uid) {
		return nil
	}
	switch name {
	case "read_file", "grep", "glob", "list_dir", "repo_map", "git":
	default:
		return nil
	}
	p, _ := args["path"].(string)
	full := absPathContext(ctx, p)
	root := filepath.Clean(executionConfig(ctx).Workspace)
	var err error
	if executionConfig(ctx).Backend == "ssh" {
		full, err = runShellContext(ctx, "realpath -e -- "+shellQuote(full), 10*time.Second)
		if err != nil {
			return fmt.Errorf("无法确认工作区路径: %w", err)
		}
		full = strings.TrimSpace(full)
		root, err = runShellContext(ctx, "realpath -e -- "+shellQuote(root), 10*time.Second)
		if err != nil {
			return fmt.Errorf("无法确认工作区: %w", err)
		}
		root = strings.TrimSpace(root)
	} else {
		full, err = filepath.EvalSymlinks(full)
		if err != nil {
			return err
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
	}
	if !pathInsideTool(root, full) {
		return fmt.Errorf("朋友只允许读取工作区内的路径")
	}
	if executionConfig(ctx).Backend != "ssh" && baseDir != "" {
		protected := []string{configPath(), filepath.Join(baseDir, "sessions"), filepath.Join(baseDir, "runs"), filepath.Join(baseDir, "agent.log"), filepath.Join(baseDir, "cron.json")}
		for _, secret := range protected {
			secret, _ = filepath.Abs(secret)
			if canonical, e := filepath.EvalSymlinks(secret); e == nil {
				secret = canonical
			}
			if pathInsideTool(secret, full) || (name != "read_file" && name != "list_dir" && pathInsideTool(full, secret)) {
				return fmt.Errorf("禁止读取机器人配置和其他会话数据，请将工作区与数据目录分开")
			}
		}
	}
	// Prevent a final symlink swap for ordinary reads by replacing it with the canonical path.
	args["path"] = full
	return nil
}
func pathInsideTool(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
