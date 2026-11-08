package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skillTestConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{SkillsDir: filepath.Join(dir, "skills"), Workspace: filepath.Join(dir, "workspace"), Backend: "local", AutoLoadSkills: true, ApprovalMode: "manual"}
}

func writeSkillTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSkillDiscoveryPrecedenceAndSymlinks(t *testing.T) {
	c := skillTestConfig(t)
	writeSkillTest(t, filepath.Join(c.SkillsDir, "code.md"), "---\nname: code\ndescription: global\n---\nglobal")
	writeSkillTest(t, filepath.Join(c.Workspace, ".skills", "code", "SKILL.md"), "---\nname: code\ndescription: legacy project\n---\nlegacy")
	projectPath := filepath.Join(c.Workspace, ".agents", "skills", "code", "SKILL.md")
	writeSkillTest(t, projectPath, "---\nname: code\ndescription: project\n---\nproject")
	outside := filepath.Join(t.TempDir(), "secret.md")
	writeSkillTest(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(c.SkillsDir, "escape.md")); err != nil {
		t.Fatal(err)
	}
	skills, err := loadSkillIndexFor(c)
	if err != nil || len(skills) != 1 || skills[0].Desc != "project" {
		t.Fatalf("skills=%+v err=%v", skills, err)
	}
	// Replacing an indexed file with a symlink must fail when it is loaded.
	if err := os.Remove(projectPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, projectPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readSkillFile(skills[0].Root, skills[0].Path); err == nil {
		t.Fatal("loaded a symlink after indexing")
	}
}

func TestSkillBoundedUTF8AndAutoSelection(t *testing.T) {
	c := skillTestConfig(t)
	writeSkillTest(t, filepath.Join(c.SkillsDir, "large.md"), strings.Repeat("x", maxSkillBytes+1))
	writeSkillTest(t, filepath.Join(c.SkillsDir, "code.md"), "---\r\nname: code-task\r\ndescription: 写代码、测试、修复 bug\r\n---\r\n先读代码，再修改并验证。")
	writeSkillTest(t, filepath.Join(c.SkillsDir, "research.md"), "---\nname: research\ndescription: >-\n  联网调研\n  搜索来源\n---\n核实来源。")
	ctx := WithExecutionConfig(context.Background(), c)
	out := autoSkillContext(ctx, "帮我修复代码中的 bug 并跑测试")
	if !strings.Contains(out, "code-task") || strings.Contains(out, "核实来源") || strings.Contains(out, strings.Repeat("x", 100)) {
		t.Fatalf("unexpected auto context: %s", out)
	}
	if got := autoSkillContext(ctx, "联网调研并搜索来源"); !strings.Contains(got, "核实来源") {
		t.Fatal("folded YAML description was not used for relevance")
	}
	c.AutoLoadSkills = false
	if got := autoSkillContext(WithExecutionConfig(context.Background(), c), "修复代码"); got != "" {
		t.Fatal("disabled automatic loading still added skills")
	}
}

func TestSkillURLPolicy(t *testing.T) {
	c := Config{SkillAllowedHosts: []string{"raw.githubusercontent.com", "127.0.0.1", "localhost", "10.1.1.1"}}
	for _, raw := range []string{"http://raw.githubusercontent.com/skill.md", "https://evil.example/skill.md", "https://user@raw.githubusercontent.com/skill.md", "https://127.0.0.1/skill.md", "https://localhost/skill.md", "https://10.1.1.1/skill.md", "https://raw.githubusercontent.com:8443/skill.md"} {
		if _, err := validateSkillURL(c, raw); err == nil {
			t.Errorf("URL should be rejected: %s", raw)
		}
	}
	if _, err := validateSkillURL(c, "https://raw.githubusercontent.com/org/repo/ref/SKILL.md"); err != nil {
		t.Fatal(err)
	}
}

func TestSkillInstallPinnedAndAtomic(t *testing.T) {
	content := "---\nname: code\ndescription: 代码修复\n---\n先读文件再改代码。\n"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://evil.example/code.md", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()
	// Route an allowed public name to the test TLS server while retaining the
	// server's test CA trust. This does not make external network requests.
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport; transport.CloseIdleConnections() })
	c := skillTestConfig(t)
	c.SkillAllowedHosts = []string{"example.com"}
	digest := sha256.Sum256([]byte(content))
	c.SkillSources = []SkillSource{{Name: "code", URL: "https://example.com/code.md", SHA256: hex.EncodeToString(digest[:])}}
	if _, err := installSkill(context.Background(), c, "code"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(c.SkillsDir, "code.md")
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("skill permission: %v %v", info, err)
	}
	content = "tampered source"
	c.SkillSources[0].SHA256 = strings.Repeat("0", 64)
	if _, err := installSkill(context.Background(), c, "code"); err == nil {
		t.Fatal("digest mismatch was installed")
	}
	got, err := os.ReadFile(dest)
	if err != nil || !strings.Contains(string(got), "代码修复") {
		t.Fatal("failed update damaged existing skill")
	}
	c.SkillSources[0].URL = "https://example.com/redirect"
	if _, err := installSkill(context.Background(), c, "code"); err == nil {
		t.Fatal("redirect to an unlisted host was accepted")
	}
}
