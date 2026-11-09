package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // daily schedules work in minimal containers without zoneinfo
)

type Provider struct {
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url"`
	APIKey       string   `json:"api_key"`
	Models       []string `json:"models"`
	DefaultModel string   `json:"default_model"`
	Efforts      []string `json:"efforts,omitempty"` // reasoning_effort values this provider accepts; empty = match by model name
}

type SSHConf struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	KeyPath string `json:"key_path"`
}

type ASRConf struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

type Config struct {
	Token                  string          `json:"token"`
	OwnerID                int64           `json:"owner_id"`
	Admins                 []int64         `json:"admins"`
	Friends                []int64         `json:"friends"`
	Banned                 []int64         `json:"banned"`
	Provider               []Provider      `json:"providers"`
	ProviderName           string          `json:"provider_name"`
	Model                  string          `json:"model"`
	SystemPrompt           string          `json:"system_prompt"`
	Persona                string          `json:"persona"`
	ReasoningEffort        string          `json:"reasoning_effort"`
	ShowReasoning          bool            `json:"show_reasoning"`
	ASR                    ASRConf         `json:"asr"`
	SkillsDir              string          `json:"skills_dir"`
	Workspace              string          `json:"workspace"`
	Backend                string          `json:"backend"`
	SSH                    SSHConf         `json:"ssh"`
	MaxSteps               int             `json:"max_steps"`
	Temperature            float64         `json:"temperature"`
	MaxHistory             int             `json:"max_history"`
	Typing                 bool            `json:"typing"`
	ShowSteps              bool            `json:"show_steps"`
	Display                map[string]bool `json:"display,omitempty"` // default display switches, see displayItems
	LLMRetries             int             `json:"llm_retries"`       // silent model API retries; 0 = default 6, -1 = none
	FriendTools            string          `json:"friend_tools"`
	QuickCommands          []QuickCmd      `json:"quick_commands"`
	Workspaces             []string        `json:"workspaces"`
	Proxy                  string          `json:"proxy"`
	NetCheckURLs           []string        `json:"net_check_urls"`
	ApprovalMode           string          `json:"approval_mode"`
	ApprovalTimeoutSeconds int             `json:"approval_timeout_seconds"`
	CommandTimeoutSeconds  int             `json:"command_timeout_seconds"`
	TaskTimeoutSeconds     int             `json:"task_timeout_seconds"`
	MaxToolOutput          int             `json:"max_tool_output"`
	MaxSubagents           int             `json:"max_subagents"`
	AutoLoadSkills         bool            `json:"auto_load_skills"`
	AutoFetchSkills        bool            `json:"auto_fetch_skills"`
	SkillSources           []SkillSource   `json:"skill_sources"`
	SkillAllowedHosts      []string        `json:"skill_allowed_hosts"`
	BotName                string          `json:"bot_name"`     // shown in menus and announcements; empty = built-in name
	Author                 string          `json:"author"`       // shown under "Support the author"
	SupportText            string          `json:"support_text"` // Markdown shown under "Support the author"
	SupportURL             string          `json:"support_url"`  // optional link button (http/https/tg)
	Registration           string          `json:"registration"` // off | approval | open
	Language               string          `json:"language"`     // auto | zh | en
	Timezone               string          `json:"timezone"`     // IANA name for daily schedules; empty = host local
}

var (
	cfg     Config // Raw configuration. Only access under cfgMu; use snapshots elsewhere.
	cfgMu   sync.RWMutex
	baseDir string

	configUpdateMu sync.Mutex
)

func configPath() string {
	if configFile != "" {
		return configFile
	}
	return filepath.Join(baseDir, "config.json")
}

func cloneConfig(c Config) Config {
	c.Admins = append([]int64(nil), c.Admins...)
	c.Friends = append([]int64(nil), c.Friends...)
	c.Banned = append([]int64(nil), c.Banned...)
	c.Provider = append([]Provider(nil), c.Provider...)
	for i := range c.Provider {
		c.Provider[i].Models = append([]string(nil), c.Provider[i].Models...)
		c.Provider[i].Efforts = append([]string(nil), c.Provider[i].Efforts...)
	}
	c.QuickCommands = append([]QuickCmd(nil), c.QuickCommands...)
	c.Workspaces = append([]string(nil), c.Workspaces...)
	c.NetCheckURLs = append([]string(nil), c.NetCheckURLs...)
	c.SkillSources = append([]SkillSource(nil), c.SkillSources...)
	c.SkillAllowedHosts = append([]string(nil), c.SkillAllowedHosts...)
	if c.Display != nil {
		display := make(map[string]bool, len(c.Display))
		for k, v := range c.Display {
			display[k] = v
		}
		c.Display = display
	}
	return c
}

func resolveSecret(v string) string {
	if strings.HasPrefix(v, "env:") {
		return os.Getenv(strings.TrimPrefix(v, "env:"))
	}
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		return os.Getenv(v[2 : len(v)-1])
	}
	return v
}

// configSnapshot returns an independent copy with secrets resolved.
func configSnapshot() Config {
	cfgMu.RLock()
	c := cloneConfig(cfg)
	cfgMu.RUnlock()
	c.Token = resolveSecret(c.Token)
	if v := os.Getenv("TG_BOT_TOKEN"); v != "" {
		c.Token = v
	}
	for i := range c.Provider {
		c.Provider[i].APIKey = resolveSecret(c.Provider[i].APIKey)
	}
	c.ASR.APIKey = resolveSecret(c.ASR.APIKey)
	return c
}

func normalizeConfig(c *Config) {
	if c.Workspace == "" {
		c.Workspace = filepath.Join(baseDir, "workspace")
	}
	if !filepath.IsAbs(c.Workspace) {
		c.Workspace = filepath.Join(baseDir, c.Workspace)
	}
	if c.SkillsDir == "" {
		c.SkillsDir = filepath.Join(baseDir, "skills")
	}
	if !filepath.IsAbs(c.SkillsDir) {
		c.SkillsDir = filepath.Join(baseDir, c.SkillsDir)
	}
	defaults := []struct {
		field *int
		value int
	}{
		{&c.MaxSteps, 40}, {&c.MaxHistory, 30}, {&c.ApprovalTimeoutSeconds, 300},
		{&c.CommandTimeoutSeconds, 120}, {&c.TaskTimeoutSeconds, 1800},
		{&c.MaxToolOutput, 24000}, {&c.MaxSubagents, 4},
	}
	for _, d := range defaults {
		if *d.field <= 0 {
			*d.field = d.value
		}
	}
	if c.Backend == "" {
		c.Backend = "local"
	}
	if c.FriendTools == "" {
		c.FriendTools = "read"
	}
	if c.ApprovalMode == "" {
		c.ApprovalMode = "manual"
	}
	if c.Language == "" {
		c.Language = "auto"
	}
	if c.Registration == "" {
		c.Registration = "approval"
	}
	if c.SSH.Port == 0 {
		c.SSH.Port = 22
	}
	if c.ASR.Model == "" {
		c.ASR.Model = "whisper-1"
	}
	if c.ProviderName == "" && len(c.Provider) > 0 {
		c.ProviderName = c.Provider[0].Name
	}
	for _, p := range c.Provider {
		if p.Name == c.ProviderName && c.Model == "" {
			c.Model = p.DefaultModel
			if c.Model == "" && len(p.Models) > 0 {
				c.Model = p.Models[0]
			}
		}
	}
}

func validateConfig(c Config) error {
	if c.OwnerID <= 0 {
		return fmt.Errorf("owner_id 必须是正整数，可先通过 BotFather/Telegram 查询自己的用户 ID")
	}
	if c.Backend != "local" && c.Backend != "ssh" {
		return fmt.Errorf("backend 必须为 local 或 ssh")
	}
	if !filepath.IsAbs(c.Workspace) {
		return fmt.Errorf("workspace 必须是绝对路径")
	}
	switch c.ApprovalMode {
	case "manual", "auto", "read-only":
	default:
		return fmt.Errorf("approval_mode 必须为 manual、auto 或 read-only")
	}
	if c.FriendTools != "read" && c.FriendTools != "all" {
		return fmt.Errorf("friend_tools 必须为 read 或 all")
	}
	switch c.Language {
	case "", "auto", langZH, langEN:
	default:
		return fmt.Errorf("language 必须为 auto、zh 或 en")
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("timezone 无效: %s", c.Timezone)
		}
	}
	if !validEffort(c.ReasoningEffort) {
		return fmt.Errorf("reasoning_effort 只能是小写字母，例如 low、medium、high")
	}
	if c.MaxSteps < 1 || c.MaxSteps > 200 || c.MaxHistory < 1 || c.MaxHistory > 200 {
		return fmt.Errorf("max_steps/max_history 必须在 1..200")
	}
	if c.CommandTimeoutSeconds < 1 || c.CommandTimeoutSeconds > 3600 || c.TaskTimeoutSeconds < 1 || c.TaskTimeoutSeconds > 86400 || c.ApprovalTimeoutSeconds < 1 || c.ApprovalTimeoutSeconds > 3600 {
		return fmt.Errorf("命令/审核超时范围 1..3600 秒，任务超时范围 1..86400 秒")
	}
	if c.MaxToolOutput < 1024 || c.MaxToolOutput > 1048576 || c.MaxSubagents < 1 || c.MaxSubagents > 16 {
		return fmt.Errorf("max_tool_output 范围 1024..1048576，max_subagents 范围 1..16")
	}
	if c.Temperature < 0 || c.Temperature > 2 {
		return fmt.Errorf("temperature 必须在 0..2")
	}
	switch c.Registration {
	case "", "off", "approval", "open":
	default:
		return fmt.Errorf("registration 必须为 off、approval 或 open")
	}
	if len([]rune(c.BotName)) > 64 || len([]rune(c.Author)) > 64 || len([]rune(c.SupportText)) > 3000 {
		return fmt.Errorf("bot_name/author 最多 64 字，support_text 最多 3000 字")
	}
	if c.SupportURL != "" && !validLinkURL(c.SupportURL) {
		return fmt.Errorf("support_url 必须是 http(s):// 或 tg:// 链接")
	}
	if c.LLMRetries < -1 || c.LLMRetries > 20 {
		return fmt.Errorf("llm_retries 范围 -1..20（0 表示默认 6 次，-1 表示不重试）")
	}
	for key := range c.Display {
		if findDisplayItem(key) == nil {
			return fmt.Errorf("display 含未知项 %q，可用：%s", key, strings.Join(displayKeys(), ", "))
		}
	}
	if c.Backend == "ssh" {
		if c.SSH.Host == "" || c.SSH.User == "" || c.SSH.Port < 1 || c.SSH.Port > 65535 || strings.HasPrefix(c.SSH.Host, "-") || strings.ContainsAny(c.SSH.Host+c.SSH.User, " \t\n\r") {
			return fmt.Errorf("SSH host/user/port 配置无效")
		}
	}
	skillNames := map[string]bool{}
	for _, source := range c.SkillSources {
		if !validSkillName(source.Name) || skillNames[source.Name] {
			return fmt.Errorf("skill_sources 名称无效或重复: %s", source.Name)
		}
		skillNames[source.Name] = true
		digest, err := hex.DecodeString(source.SHA256)
		if err != nil || len(digest) != 32 {
			return fmt.Errorf("技能源 %s 需要64位SHA256十六进制摘要", source.Name)
		}
		if _, err := validateSkillURL(c, source.URL); err != nil {
			return fmt.Errorf("技能源 %s: %w", source.Name, err)
		}
	}
	names := map[string]bool{}
	selected := false
	for _, p := range c.Provider {
		if p.Name == "" || names[p.Name] {
			return fmt.Errorf("provider 名称不能为空或重复")
		}
		names[p.Name] = true
		u, e := url.Parse(p.BaseURL)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("provider %s 的 base_url 无效", p.Name)
		}
		if p.Name == c.ProviderName {
			selected = true
		}
		for _, e := range p.Efforts {
			if e = strings.ToLower(strings.TrimSpace(e)); e != "off" && (e == "" || !validEffort(e)) {
				return fmt.Errorf("provider %s 的 efforts 只能是小写字母档位，例如 low、medium、high", p.Name)
			}
		}
	}
	if len(c.Provider) > 0 && !selected {
		return fmt.Errorf("provider_name 不在 providers 中")
	}
	return nil
}

// validEffort accepts provider-specific effort names such as low/high/xhigh;
// an empty value omits the parameter entirely.
func validEffort(v string) bool {
	if len(v) > 16 {
		return false
	}
	for _, r := range v {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func loadConfig() error {
	b, e := os.ReadFile(configPath())
	if e != nil {
		return e
	}
	var c Config
	if e = json.Unmarshal(b, &c); e != nil {
		return e
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["auto_load_skills"]; !ok {
		c.AutoLoadSkills = true
	}
	normalizeConfig(&c)
	if e = validateConfig(c); e != nil {
		return e
	}
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	return nil
}

// preserveSecret keeps environment references when a settings form saves an effective snapshot.
func preserveSecret(old, next string) string {
	if (strings.HasPrefix(old, "env:") || strings.HasPrefix(old, "${")) && resolveSecret(old) == next {
		return old
	}
	return next
}

func saveConfig(next ...Config) error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	c := cloneConfig(cfg)
	if len(next) > 0 {
		c = cloneConfig(next[0])
	}
	c.Token = preserveSecret(cfg.Token, c.Token)
	if v := os.Getenv("TG_BOT_TOKEN"); v != "" && c.Token == v {
		c.Token = cfg.Token
	}
	for i := range c.Provider {
		for _, p := range cfg.Provider {
			if p.Name == c.Provider[i].Name {
				c.Provider[i].APIKey = preserveSecret(p.APIKey, c.Provider[i].APIKey)
			}
		}
	}
	c.ASR.APIKey = preserveSecret(cfg.ASR.APIKey, c.ASR.APIKey)
	normalizeConfig(&c)
	if e := validateConfig(c); e != nil {
		return e
	}
	b, e := json.MarshalIndent(c, "", "  ")
	if e != nil {
		return e
	}
	if e = atomicWriteFile(configPath(), b, 0600); e != nil {
		return e
	}
	cfg = c
	return nil
}

// updateConfig applies fn to a fresh snapshot and saves it atomically.
func updateConfig(fn func(*Config)) error {
	configUpdateMu.Lock()
	defer configUpdateMu.Unlock()
	c := configSnapshot()
	fn(&c)
	return saveConfig(c)
}

func scheduleLocation(c Config) *time.Location {
	if c.Timezone != "" {
		if loc, err := time.LoadLocation(c.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}
