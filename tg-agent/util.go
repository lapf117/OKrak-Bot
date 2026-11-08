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
		logFile.WriteString(line)
	}
}

func openLog() {
	logFile, _ = os.OpenFile(filepath.Join(baseDir, "agent.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
}

func short(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// envOrDie 保证关键配置存在
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

// Redact configured credentials before error messages and audit data leave the process.
func redactSecrets(s string) string {
	c := configSnapshot()
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
	for _, secret := range secrets {
		if len(secret) >= 6 {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return s
}
