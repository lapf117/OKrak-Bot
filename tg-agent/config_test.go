package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFixture(t *testing.T) Config {
	t.Helper()
	c := Config{Token: "test-token-value", OwnerID: 101, Provider: []Provider{{Name: "test", BaseURL: "https://example.com/v1", APIKey: "test-api-key", Models: []string{"test-model"}}}, Workspace: t.TempDir(), Temperature: 0.7}
	normalizeConfig(&c)
	return c
}

func TestConfigSnapshotsAreIndependent(t *testing.T) {
	previous := configSnapshot()
	t.Cleanup(func() { cfgMu.Lock(); cfg = previous; cfgMu.Unlock() })
	c := configFixture(t)
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	got := configSnapshot()
	got.Provider[0].Models[0] = "changed"
	got.Provider[0].Name = "changed"
	if next := configSnapshot(); next.Provider[0].Models[0] != "test-model" || next.Provider[0].Name != "test" {
		t.Fatal("snapshot aliases mutable configuration")
	}
}
func TestConfigSaveKeepsEnvironmentReferencesAndRejectsInvalid(t *testing.T) {
	previous := configSnapshot()
	oldFile := configFile
	t.Cleanup(func() { cfgMu.Lock(); cfg = previous; cfgMu.Unlock(); configFile = oldFile })
	configFile = filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TG_TEST_SECRET", "a-long-test-secret-value")
	c := configFixture(t)
	c.Provider[0].APIKey = "env:TG_TEST_SECRET"
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	effective := configSnapshot()
	if effective.Provider[0].APIKey != "a-long-test-secret-value" {
		t.Fatal("environment secret was not resolved")
	}
	effective.Persona = "new persona"
	if err := saveConfig(effective); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "a-long-test-secret-value") || !strings.Contains(string(data), "env:TG_TEST_SECRET") {
		t.Fatal("save materialized secret")
	}
	info, _ := os.Stat(configFile)
	if info.Mode().Perm() != 0600 {
		t.Fatal("config must be private")
	}
	effective.ApprovalMode = "invalid"
	if err := saveConfig(effective); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	if got := configSnapshot(); got.ApprovalMode != "manual" {
		t.Fatal("invalid configuration applied in memory")
	}
}
func TestConfigDefaultsAndValidation(t *testing.T) {
	previous := configSnapshot()
	oldFile := configFile
	t.Cleanup(func() { cfgMu.Lock(); cfg = previous; cfgMu.Unlock(); configFile = oldFile })
	configFile = filepath.Join(t.TempDir(), "config.json")
	c := configFixture(t)
	raw, _ := json.Marshal(c)
	var fields map[string]interface{}
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "auto_load_skills")
	raw, _ = json.Marshal(fields)
	if err := os.WriteFile(configFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	if !configSnapshot().AutoLoadSkills {
		t.Fatal("automatic local skill loading should default on")
	}
	c.OwnerID = 0
	if validateConfig(c) == nil {
		t.Fatal("unowned bot accepted")
	}
}
func TestLegacyCronWithoutCreatorIsDisabled(t *testing.T) {
	previousBase := baseDir
	cronMu.Lock()
	previousJobs := cronJobs
	previousSeq := cronSeq
	cronMu.Unlock()
	t.Cleanup(func() {
		baseDir = previousBase
		cronMu.Lock()
		cronJobs = previousJobs
		cronSeq = previousSeq
		cronMu.Unlock()
	})
	baseDir = t.TempDir()
	if err := os.WriteFile(cronPath(), []byte(`[{"id":9,"chat_id":101,"every":60,"prompt":"do it","enabled":true}]`), 0600); err != nil {
		t.Fatal(err)
	}
	loadCron()
	cronMu.Lock()
	defer cronMu.Unlock()
	if len(cronJobs) != 1 || cronJobs[0].Enabled {
		t.Fatal("legacy schedule must not be elevated to owner")
	}
}
