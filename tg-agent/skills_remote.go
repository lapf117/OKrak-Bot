package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Remote skills: pinned-hash installation from operator-approved hosts.

func validateSkillURL(c Config, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, fmt.Errorf("技能源必须是 HTTPS URL（443 端口，无用户信息）")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return nil, fmt.Errorf("技能源不允许本机地址")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
		return nil, fmt.Errorf("技能源不允许私网地址")
	}
	for _, allowedHost := range c.SkillAllowedHosts {
		if strings.EqualFold(strings.TrimSpace(allowedHost), host) {
			return u, nil
		}
	}
	return nil, fmt.Errorf("技能源主机 %s 不在 skill_allowed_hosts 中", host)
}

func installSkill(ctx context.Context, c Config, name string) (string, error) {
	if !validSkillName(name) {
		return "", fmt.Errorf("技能名称只允许字母、数字、-、_，最长 64 字节")
	}
	var source *SkillSource
	for i := range c.SkillSources {
		if c.SkillSources[i].Name == name {
			source = &c.SkillSources[i]
			break
		}
	}
	if source == nil {
		return "", fmt.Errorf("未配置技能源 %s；请先由管理员加入 skill_sources", name)
	}
	want, err := hex.DecodeString(source.SHA256)
	if err != nil || len(want) != sha256.Size {
		return "", fmt.Errorf("技能源 %s 缺少有效 SHA256 固定摘要", name)
	}
	u, err := validateSkillURL(c, source.URL)
	if err != nil {
		return "", err
	}
	skillInstallMu.Lock()
	defer skillInstallMu.Unlock()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err := os.MkdirAll(c.SkillsDir, 0700); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(c.SkillsDir)
	if err != nil {
		return "", err
	}
	root, _ = filepath.Abs(root)
	dest := filepath.Join(root, name+".md")
	if existing, err := readSkillFile(root, dest); err == nil {
		got := sha256.Sum256(existing)
		if strings.EqualFold(hex.EncodeToString(got[:]), source.SHA256) {
			return "技能已是固定版本: " + name, nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "TG-Agent/2 skill-manager")
	client := *netClientContext(WithExecutionConfig(ctx, c))
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return fmt.Errorf("技能下载重定向过多")
		}
		_, err := validateSkillURL(c, req.URL.String())
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载技能: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载技能 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSkillBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > maxSkillBytes || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return "", fmt.Errorf("技能必须是有效 UTF-8 文本且不超过 128 KiB")
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "html") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(string(data))), "<!doctype html") {
		return "", fmt.Errorf("技能源返回网页，必须使用原始 Markdown URL")
	}
	got := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(got[:]), source.SHA256) {
		return "", fmt.Errorf("技能 %s SHA256 不匹配，未安装", name)
	}
	skill := parseSkill(data, name)
	if skill.Name != name {
		return "", fmt.Errorf("技能 frontmatter 名称 %q 与配置 %q 不一致", skill.Name, name)
	}
	f, err := os.CreateTemp(root, ".skill-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		return "", err
	}
	return "已安装并校验技能: " + name + "\nSHA256: " + source.SHA256, nil
}
