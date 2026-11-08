package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	logFile *os.File
	logMu   sync.Mutex
)

func logf(format string, a ...interface{}) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("01-02 15:04:05"), redactSecrets(fmt.Sprintf(format, a...)))
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Print(line)
	if logFile != nil {
		_, _ = logFile.WriteString(line)
	}
}

func openLog() {
	logFile, _ = os.OpenFile(filepath.Join(baseDir, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
}

// short truncates by runes and marks the cut with an ellipsis.
func short(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// oneLine collapses whitespace so values fit on a single status line.
func oneLine(s string, n int) string {
	return short(strings.Join(strings.Fields(s), " "), n)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func maxPositive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

// argsAfter returns the text that follows the first n whitespace-separated
// fields. Unlike slicing by len("add "+name) it tolerates repeated spaces.
func argsAfter(text string, n int) string {
	rest := strings.TrimLeft(text, " \t\r\n")
	for i := 0; i < n; i++ {
		end := strings.IndexAny(rest, " \t\r\n")
		if end < 0 {
			return ""
		}
		rest = strings.TrimLeft(rest[end:], " \t\r\n")
	}
	return strings.TrimSpace(rest)
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// clock renders elapsed time as m:ss for live progress panels.
func clock(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// mustConfig ensures the settings required to talk to Telegram and a model exist.
func mustConfig() error {
	c := configSnapshot()
	if err := validateConfig(c); err != nil {
		return err
	}
	if c.Token == "" || strings.HasPrefix(c.Token, "PASTE") || strings.Contains(c.Token, "YOUR_") {
		return fmt.Errorf("请在配置 token 或 TG_BOT_TOKEN 环境变量中设置 Telegram Bot Token")
	}
	if providerFor(c) == nil {
		return fmt.Errorf("请配置至少一个 OpenAI 兼容服务商")
	}
	for _, p := range c.Provider {
		if p.Name == c.ProviderName && p.APIKey == "" {
			return fmt.Errorf("当前服务商 api_key 为空，请检查环境变量引用")
		}
	}
	if c.Model == "" || strings.Contains(c.Model, "YOUR_") {
		return fmt.Errorf("请配置当前模型名称")
	}
	return nil
}

// atomicWriteFile never exposes a partial JSON/session file to another process.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tg-agent-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

// secretsOf lists the credentials that must never leave the process.
func secretsOf(c Config) []string {
	secrets := []string{c.Token, c.ASR.APIKey}
	for _, p := range c.Provider {
		secrets = append(secrets, p.APIKey)
	}
	if c.Proxy != "" {
		if u, e := url.Parse(c.Proxy); e == nil && u.User != nil {
			if pass, ok := u.User.Password(); ok {
				secrets = append(secrets, pass)
			}
		}
	}
	return secrets
}

func redactWith(s string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) >= 6 {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return s
}

// Redact configured credentials before error messages and audit data leave the process.
func redactSecrets(s string) string { return redactWith(s, secretsOf(configSnapshot())) }
