package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func toolTestContext(t *testing.T) context.Context {
	t.Helper()
	return withTodoContext(WithExecutionConfig(context.Background(), Config{
		Workspace: t.TempDir(), Backend: "local", MaxToolOutput: 512, CommandTimeoutSeconds: 5,
	}))
}

func TestShellCancellationKillsChildPipes(t *testing.T) {
	ctx := toolTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(60*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	started := time.Now()
	_, err := runShellContext(ctx, "sleep 30 & wait", 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancellation waited for descendant pipe")
	}
}

func TestShellOutputBounded(t *testing.T) {
	ctx := toolTestContext(t)
	out, err := runShellContext(ctx, "head -c 100000 /dev/zero | tr '\\0' x", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 1024 || !strings.Contains(out, "截断") {
		t.Fatalf("unbounded or missing marker: %d", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatal("output broke UTF-8")
	}
}

func TestEditRefusesPartialRead(t *testing.T) {
	ctx := toolTestContext(t)
	path := filepath.Join(executionConfig(ctx).Workspace, "large.txt")
	original := strings.Repeat("a", maxEditableFile+50) + "needle"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := toolEditFileContext(ctx, map[string]interface{}{"path": path, "old_string": "needle", "new_string": "changed"})
	if !errors.Is(err, errOutputTruncated) {
		t.Fatalf("want truncation refusal, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != original {
		t.Fatal("oversized file was modified")
	}
}

func TestAtomicEditsAndUndoRetainEachVersion(t *testing.T) {
	ctx := toolTestContext(t)
	path := filepath.Join(executionConfig(ctx).Workspace, "quoted ' 文件.txt")
	for _, content := range []string{"一\n", "二\n", "三\n"} {
		if _, err := toolWriteFileContext(ctx, map[string]interface{}{"path": path, "content": content}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"二\n", "一\n"} {
		if _, err := toolUndoContext(ctx, map[string]interface{}{}); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != want {
			t.Fatalf("undo got %q, %v", after, err)
		}
	}
	if _, err := toolUndoContext(ctx, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("undo new file: %v", err)
	}
}

func TestTodoIsolationAndReadOnlyClassification(t *testing.T) {
	one, two := toolTestContext(t), toolTestContext(t)
	if _, err := toolTodoContext(one, map[string]interface{}{"action": "add", "text": "private task"}); err != nil {
		t.Fatal(err)
	}
	out, _ := toolTodoContext(two, map[string]interface{}{"action": "list"})
	if strings.Contains(out, "private") {
		t.Fatal("todo leaked across tasks")
	}
	if toolReadOnly("bash", `{"command":"pwd"}`) || toolReadOnly("git", `{"action":"commit"}`) || toolReadOnly("unknown", `{}`) {
		t.Fatal("mutable tool classified read only")
	}
	if !toolReadOnly("git", `{"action":"status"}`) || !toolReadOnly("todo", `{"action":"add"}`) {
		t.Fatal("safe structured tool rejected")
	}
}

func TestGrepLeadingDashIsPattern(t *testing.T) {
	ctx := toolTestContext(t)
	dir := executionConfig(ctx).Workspace
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("-n needle\nordinary\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := toolGrepContext(ctx, map[string]interface{}{"pattern": "-n", "path": dir})
	if err != nil || !strings.Contains(out, "-n needle") {
		t.Fatalf("leading-dash pattern: %q %v", out, err)
	}
}

func TestFriendPathTraversalAndSymlinkEscape(t *testing.T) {
	ctx := toolTestContext(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateToolPathContext(ctx, "read_file", map[string]interface{}{"path": secret}); err == nil {
		t.Fatal("absolute escape accepted")
	}
	link := filepath.Join(executionConfig(ctx).Workspace, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := validateToolPathContext(ctx, "read_file", map[string]interface{}{"path": filepath.Join(link, "secret.txt")}); err == nil {
		t.Fatal("symlink escape accepted")
	}
	allowed := filepath.Join(executionConfig(ctx).Workspace, "visible.txt")
	if err := os.WriteFile(allowed, []byte("visible"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateToolPathContext(ctx, "read_file", map[string]interface{}{"path": allowed}); err != nil {
		t.Fatalf("workspace path denied: %v", err)
	}
}

func TestToolSchemasUseCapturedPolicyAndRestrictGitActions(t *testing.T) {
	captured := Config{FriendTools: "all", ApprovalMode: "read-only"}
	ctx := WithExecutionConfig(context.Background(), captured)
	foundGit := false
	for _, schema := range toolSchemasContext(ctx, 0) {
		switch schema.Function.Name {
		case "bash", "write_file", "edit_file", "undo", "spawn_agent", "install_skill":
			t.Fatalf("mutable schema exposed in read-only mode: %s", schema.Function.Name)
		case "git":
			foundGit = true
			params := schema.Function.Parameters.(map[string]interface{})
			props := params["properties"].(map[string]interface{})
			action := props["action"].(map[string]interface{})
			choices := action["enum"].([]string)
			joined := strings.Join(choices, ",")
			if len(choices) != len(gitReadActions) || strings.Contains(joined, "commit") || strings.Contains(joined, "add") {
				t.Fatalf("unexpected git actions: %v", choices)
			}
		}
	}
	if !foundGit {
		t.Fatal("read-only git query unavailable")
	}
	captured.ApprovalMode = "auto"
	ctx = WithExecutionConfig(context.Background(), captured)
	foundBash := false
	for _, schema := range toolSchemasContext(ctx, 0) {
		if schema.Function.Name == "bash" {
			foundBash = true
		}
	}
	if !foundBash {
		t.Fatal("captured trusted-friend all policy not applied")
	}
}
