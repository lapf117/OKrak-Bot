package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UsageStats is the bot's cumulative accounting, persisted to usage.json.
type UsageStats struct {
	Tasks            int64 `json:"tasks"`
	ToolCalls        int64 `json:"tool_calls"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Since            int64 `json:"since"`
}

func (u UsageStats) total() int64 { return u.PromptTokens + u.CompletionTokens }

var usageMu sync.Mutex

func usagePath() string { return filepath.Join(baseDir, "usage.json") }

func loadUsage() UsageStats {
	var u UsageStats
	if raw, err := os.ReadFile(usagePath()); err == nil {
		_ = json.Unmarshal(raw, &u)
	}
	if u.Since == 0 {
		u.Since = time.Now().Unix()
	}
	return u
}

// recordTaskUsage adds one finished task's counters to the running totals.
func recordTaskUsage(tools, prompt, completion int64) {
	usageMu.Lock()
	defer usageMu.Unlock()
	u := loadUsage()
	u.Tasks++
	u.ToolCalls += tools
	u.PromptTokens += prompt
	u.CompletionTokens += completion
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return
	}
	if err := atomicWriteFile(usagePath(), data, 0600); err != nil {
		logf("保存用量失败: %v", err)
	}
}

func getUsage() UsageStats {
	usageMu.Lock()
	defer usageMu.Unlock()
	return loadUsage()
}

// ---------------- provider balance (DeepSeek-compatible) ----------------

type balanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

type balanceResponse struct {
	IsAvailable  bool          `json:"is_available"`
	BalanceInfos []balanceInfo `json:"balance_infos"`
}

// balanceURL derives the official balance endpoint from a provider base_url.
// DeepSeek serves it at the host root (/user/balance), not under /v1.
func balanceURL(baseURL string) (string, bool) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host + "/user/balance", true
}

// providerBalance queries the current provider's balance endpoint. It works
// for DeepSeek and any OpenAI-compatible service exposing the same shape.
func providerBalance(ctx context.Context, c Config) (*balanceResponse, error) {
	p := providerFor(c)
	if p == nil {
		return nil, fmt.Errorf("没有配置服务商")
	}
	endpoint, ok := balanceURL(p.BaseURL)
	if !ok {
		return nil, fmt.Errorf("无法从 base_url 解析余额地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "application/json")
	cl := netClientContext(WithExecutionConfig(ctx, c))
	defer cl.CloseIdleConnections()
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s", redactSecrets(err.Error()))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("该服务商未提供余额查询接口（%s）", p.Name)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("余额查询 HTTP %d: %s", resp.StatusCode, short(redactSecrets(string(body)), 160))
	}
	var r balanceResponse
	if err := json.Unmarshal(body, &r); err != nil || len(r.BalanceInfos) == 0 {
		return nil, fmt.Errorf("未能解析余额响应（该服务商可能不兼容 DeepSeek 余额接口）")
	}
	return &r, nil
}

func balanceText(r *balanceResponse) string {
	var parts []string
	for _, b := range r.BalanceInfos {
		cur := b.Currency
		if cur == "" {
			cur = "?"
		}
		seg := fmt.Sprintf("%s %s", b.TotalBalance, cur)
		var extra []string
		if b.ToppedUpBalance != "" && b.ToppedUpBalance != "0" {
			extra = append(extra, "充值 "+b.ToppedUpBalance)
		}
		if b.GrantedBalance != "" && b.GrantedBalance != "0" {
			extra = append(extra, "赠送 "+b.GrantedBalance)
		}
		if len(extra) > 0 {
			seg += " (" + strings.Join(extra, " · ") + ")"
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, " · ")
}

// ---------------- usage panel ----------------

func usagePanel(uid int64, lang string) string {
	c := configSnapshot()
	u := getUsage()
	var b strings.Builder
	b.WriteString("<b>" + esc(tr(lang, "usage.title")) + "</b>\n\n")
	fmt.Fprintf(&b, "%s <code>%s</code> · %s %s\n\n", esc(tr(lang, "menu.model")), esc(c.Model), esc(tr(lang, "menu.effort")), esc(effortLabel(lang, c.ReasoningEffort)))
	fmt.Fprintf(&b, "%s <b>%s</b> tokens\n", esc(tr(lang, "usage.total")), esc(humanTokens(u.total())))
	fmt.Fprintf(&b, "  · %s %s\n  · %s %s\n", esc(tr(lang, "usage.prompt")), esc(humanTokens(u.PromptTokens)), esc(tr(lang, "usage.completion")), esc(humanTokens(u.CompletionTokens)))
	fmt.Fprintf(&b, "%s %s · %s %s\n", esc(tr(lang, "usage.tasks")), esc(humanTokens(u.Tasks)), esc(tr(lang, "usage.tools")), esc(humanTokens(u.ToolCalls)))
	if u.Since > 0 {
		fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "usage.since")), esc(time.Unix(u.Since, 0).Format("2006-01-02")))
	}
	if tok := lastUsageTokens(uid); tok > 0 {
		fmt.Fprintf(&b, "%s %s tokens\n", esc(tr(lang, "usage.last")), esc(humanTokens(tok)))
	}
	// Balance is a live call; only admins see it and only when supported.
	if isAdmin(uid) {
		ctx, cancel := context.WithTimeout(telegramContext, 12*time.Second)
		defer cancel()
		if bal, err := providerBalance(ctx, c); err == nil {
			status := tr(lang, "usage.balance_ok")
			if !bal.IsAvailable {
				status = tr(lang, "usage.balance_low")
			}
			fmt.Fprintf(&b, "\n<b>%s</b> %s\n%s\n", esc(tr(lang, "usage.balance")), esc(status), esc(balanceText(bal)))
		} else {
			fmt.Fprintf(&b, "\n<b>%s</b> <i>%s</i>\n", esc(tr(lang, "usage.balance")), esc(short(err.Error(), 120)))
		}
	}
	return b.String()
}

func usageMenu(lang string, admin bool) interface{} {
	rows := [][]Btn{}
	if admin {
		rows = append(rows, []Btn{btn(tr(lang, "btn.refresh"), "act:usage")})
	}
	rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:root")})
	return keyboard(rows)
}

func handleUsageCommand(chatID, uid int64) {
	sendUI(chatID, usagePanel(uid, langFor(uid)), usageMenu(langFor(uid), isAdmin(uid)))
}
