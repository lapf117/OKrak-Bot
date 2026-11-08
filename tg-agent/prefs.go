package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UserPrefs are per-user UI choices stored in prefs.json next to config.json.
type UserPrefs struct {
	Lang  string          `json:"lang,omitempty"`  // zh | en; empty follows config/Telegram
	Steps string          `json:"steps,omitempty"` // on | off; empty follows show_steps
	Show  map[string]bool `json:"show,omitempty"`  // display switches overriding the defaults
}

func (p UserPrefs) empty() bool { return p.Lang == "" && p.Steps == "" && len(p.Show) == 0 }

// clone copies the map so callers never share state with prefsState.
func (p UserPrefs) clone() UserPrefs {
	if p.Show != nil {
		show := make(map[string]bool, len(p.Show))
		for k, v := range p.Show {
			show[k] = v
		}
		p.Show = show
	}
	return p
}

var prefsState = struct {
	sync.Mutex
	path string
	data map[int64]UserPrefs
}{}

func prefsPath() string { return filepath.Join(baseDir, "prefs.json") }

// loadPrefsLocked lazily (re)loads when the data directory changes.
func loadPrefsLocked() {
	path := prefsPath()
	if prefsState.data != nil && prefsState.path == path {
		return
	}
	prefsState.path = path
	prefsState.data = map[int64]UserPrefs{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &prefsState.data); err != nil {
			logf("用户偏好文件损坏，已忽略: %v", err)
			prefsState.data = map[int64]UserPrefs{}
		}
	}
}

func getPrefs(uid int64) UserPrefs {
	prefsState.Lock()
	defer prefsState.Unlock()
	loadPrefsLocked()
	return prefsState.data[uid].clone()
}

func updatePrefs(uid int64, fn func(*UserPrefs)) error {
	prefsState.Lock()
	defer prefsState.Unlock()
	loadPrefsLocked()
	p := prefsState.data[uid].clone()
	fn(&p)
	if len(p.Show) == 0 {
		p.Show = nil
	}
	if p.empty() {
		delete(prefsState.data, uid)
	} else {
		prefsState.data[uid] = p
	}
	data, err := json.MarshalIndent(prefsState.data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(prefsState.path, data, 0600)
}

// ---------------- seen users ----------------

type SeenUser struct {
	Name     string
	Last     int64
	LangCode string
}

var (
	seenMu sync.Mutex
	seen   = map[int64]SeenUser{}
)

func touchUser(u User) {
	seenMu.Lock()
	defer seenMu.Unlock()
	s := seen[u.ID]
	s.Name, s.Last = displayName(u), time.Now().Unix()
	if u.LanguageCode != "" {
		s.LangCode = u.LanguageCode
	}
	seen[u.ID] = s
}

func userName(uid int64) string {
	seenMu.Lock()
	defer seenMu.Unlock()
	if s, ok := seen[uid]; ok && s.Name != "" {
		return s.Name
	}
	return "?"
}

func lastActive(uid int64) string {
	seenMu.Lock()
	defer seenMu.Unlock()
	if s, ok := seen[uid]; ok && s.Last > 0 {
		return time.Unix(s.Last, 0).Format("01-02 15:04")
	}
	return "-"
}

// langFor picks the UI language: explicit user choice, then the configured
// language, then the Telegram client language. Chinese is the fallback.
func langFor(uid int64) string {
	if p := getPrefs(uid); p.Lang == langZH || p.Lang == langEN {
		return p.Lang
	}
	if l := configSnapshot().Language; l == langZH || l == langEN {
		return l
	}
	seenMu.Lock()
	code := strings.ToLower(seen[uid].LangCode)
	seenMu.Unlock()
	if code == "" || strings.HasPrefix(code, "zh") {
		return langZH
	}
	return langEN
}

// stepsDetailed reports whether the live panel should include tool output previews.
func stepsDetailed(uid int64, c Config) bool {
	switch getPrefs(uid).Steps {
	case "on":
		return true
	case "off":
		return false
	}
	if v, ok := c.Display["output"]; ok {
		return v
	}
	return c.ShowSteps
}
