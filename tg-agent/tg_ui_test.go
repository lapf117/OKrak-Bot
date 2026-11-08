package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

type uiRoundTripper func(*http.Request) (*http.Response, error)

func (f uiRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramUTF16SplitsWithoutLosingContent(t *testing.T) {
	original := strings.Repeat("你😀", 1800) + "\n任务结束"
	chunks := splitTelegramText(original, 4000)
	if len(chunks) < 2 {
		t.Fatal("expected multiple Telegram messages")
	}
	if strings.Join(chunks, "") != original {
		t.Fatal("message content lost during splitting")
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatal("invalid UTF-8 chunk")
		}
		if len(utf16.Encode([]rune(chunk))) > 4000 {
			t.Fatal("Telegram UTF-16 limit exceeded")
		}
	}
	// Previously a large footer with a short answer sliced beyond answer[3700:].
	shortAnswer := "已完成" + strings.Repeat("x", 4100)
	if strings.Join(splitTelegramText(shortAnswer, 4000), "") != shortAnswer {
		t.Fatal("short answer/footer combination lost")
	}
}

func TestSendMessageDeliversAllUTF16Chunks(t *testing.T) {
	previous := httpClient
	defer func() { httpClient = previous }()
	var sent []string
	httpClient = &http.Client{Transport: uiRoundTripper(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, payload.Text)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":7}}`)), Header: make(http.Header)}, nil
	})}
	original := strings.Repeat("😀", 4100)
	if id := sendMessage(1, original, nil); id != 7 {
		t.Fatalf("wrong message id %d", id)
	}
	if strings.Join(sent, "") != original {
		t.Fatal("sendMessage truncated output")
	}
	for _, part := range sent {
		if len(utf16.Encode([]rune(part))) > 4000 {
			t.Fatal("UTF16 limit exceeded")
		}
	}
}

func TestDeliverableRejectsEscapeAndPrivateFiles(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "project")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	previousBase, previousUploads, previousConfig := baseDir, uploadsDir, configFile
	cfgMu.Lock()
	previousCfg := cfg
	cfg = Config{Workspace: workspace}
	cfgMu.Unlock()
	baseDir, uploadsDir, configFile = root, filepath.Join(root, "uploads"), filepath.Join(root, "config.json")
	defer func() {
		baseDir, uploadsDir, configFile = previousBase, previousUploads, previousConfig
		cfgMu.Lock()
		cfg = previousCfg
		cfgMu.Unlock()
	}()
	normal := filepath.Join(workspace, "report.txt")
	os.WriteFile(normal, []byte("hello"), 0600)
	data, err := readDeliverable(normal)
	if err != nil || string(data) != "hello" {
		t.Fatalf("normal deliverable: %q %v", data, err)
	}
	private := filepath.Join(workspace, ".env")
	os.WriteFile(private, []byte("secret"), 0600)
	outside := filepath.Join(root, "outside.txt")
	os.WriteFile(outside, []byte("private"), 0600)
	link := filepath.Join(workspace, "leak.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{private, outside, link, workspace, "relative.txt"} {
		if _, err := readDeliverable(path); err == nil {
			t.Fatalf("unexpected delivery allowed: %s", path)
		}
	}
}

func TestNormalizeBotCommandPreservesArguments(t *testing.T) {
	for input, want := range map[string]string{"/MODEL@my_bot 中文 模型": "/MODEL 中文 模型", "/menu@my_bot": "/menu", "普通 @文本": "普通 @文本"} {
		if got := normalizeBotCommand(input); got != want {
			t.Fatalf("%q => %q, want %q", input, got, want)
		}
	}
}
