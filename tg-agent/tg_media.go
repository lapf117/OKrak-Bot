package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxSendBytes = 48 << 20

// ---------------- download ----------------

func tgFilePath(fileID string) string {
	raw := tgCall("getFile", map[string]interface{}{"file_id": fileID})
	if raw == nil {
		return ""
	}
	var r struct {
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.FilePath
}

func downloadTGFile(p string) ([]byte, error) {
	c := configSnapshot()
	if p == "" {
		return nil, fmt.Errorf("没有 file_path（Bot API 只能下载 20MB 以内的文件）")
	}
	req, err := http.NewRequestWithContext(telegramContext, http.MethodGet, "https://api.telegram.org/file/bot"+c.Token+"/"+p, nil)
	if err != nil {
		return nil, fmt.Errorf("%s", redactSecrets(err.Error()))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s", redactSecrets(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20+1))
	if len(data) > 20<<20 {
		return nil, fmt.Errorf("下载文件超过 20MB")
	}
	return data, err
}

// ---------------- upload ----------------

// sendMedia uploads bytes with multipart/form-data. Captions are plain text.
func sendMedia(chatID int64, endpoint, field string, data []byte, name, caption string) bool {
	c := configSnapshot()
	if len(data) == 0 || len(data) > maxSendBytes {
		return false
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		_ = w.WriteField("caption", truncateTelegramText(redactSecrets(caption), 1000))
	}
	fw, err := w.CreateFormFile(field, name)
	if err != nil {
		return false
	}
	_, _ = fw.Write(data)
	_ = w.Close()
	req, err := http.NewRequestWithContext(telegramContext, http.MethodPost, apiBase+c.Token+"/"+endpoint, &buf)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := httpClient.Do(req)
	if err != nil {
		logf("%s 失败: %s", endpoint, redactSecrets(err.Error()))
		return false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r tgResp
	_ = json.Unmarshal(raw, &r)
	if !r.OK {
		logf("%s 拒绝: %s", endpoint, redactSecrets(r.Description))
	}
	return r.OK
}

var imgExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true}
var vidExt = map[string]bool{".mp4": true, ".mov": true, ".mkv": true, ".webm": true}

func sendByExt(chatID int64, path string, data []byte) bool {
	return sendFileBytes(chatID, path, data, filepath.Base(path))
}

// sendFileBytes picks photo/video/document by extension; photos larger than
// Telegram's 10MB photo limit go out as documents.
func sendFileBytes(chatID int64, path string, data []byte, caption string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	name := filepath.Base(path)
	switch {
	case imgExt[ext] && ext != ".bmp" && len(data) <= 10<<20:
		return sendMedia(chatID, "sendPhoto", "photo", data, name, caption)
	case vidExt[ext]:
		return sendMedia(chatID, "sendVideo", "video", data, name, caption)
	default:
		return sendMedia(chatID, "sendDocument", "document", data, name, caption)
	}
}

// Resolve symlinks before checking roots so a generated path cannot expose host files.
func readDeliverable(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("请使用绝对路径")
	}
	full, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	full, err = filepath.Abs(full)
	if err != nil {
		return nil, err
	}
	c := configSnapshot()
	trusted := false
	for _, root := range []string{c.Workspace, uploadsDir, filepath.Join(baseDir, "runs")} {
		if root == "" {
			continue
		}
		realRoot, e := filepath.EvalSymlinks(root)
		if e != nil {
			continue
		}
		if pathInside(realRoot, full) {
			trusted = true
			break
		}
	}
	if !trusted {
		return nil, fmt.Errorf("只允许发送工作区、上传目录或执行记录中的文件")
	}
	for _, part := range strings.Split(filepath.ToSlash(full), "/") {
		lower := strings.ToLower(part)
		if lower == ".ssh" || lower == ".aws" || lower == ".gnupg" || lower == ".config" || lower == ".git" || lower == "config.json" || lower == "agent.log" || lower == ".env" || strings.HasPrefix(lower, ".env.") || strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".pem") || lower == "credentials" {
			return nil, fmt.Errorf("该路径包含私密配置，不能通过聊天发送")
		}
	}
	if configFull, _ := filepath.Abs(configPath()); full == configFull {
		return nil, fmt.Errorf("不能发送服务配置")
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("只支持普通文件")
	}
	if info.Size() > maxSendBytes {
		return nil, fmt.Errorf("文件超过发送上限 48MB")
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSendBytes+1))
	if len(data) > maxSendBytes {
		return nil, fmt.Errorf("文件超过发送上限 48MB")
	}
	return data, err
}

var mediaRe = regexp.MustCompile(`(?i)(/[^\s'"<>()\x60*]+\.(?:png|jpe?g|gif|webp|bmp|mp4|mov|mkv|webm))`)

// sendMediaInText delivers local media paths mentioned in a reply, skipping
// files the agent already sent with send_file.
func sendMediaInText(chatID int64, text string, already map[string]bool) {
	seenPaths := map[string]bool{}
	n := 0
	for _, m := range mediaRe.FindAllString(text, -1) {
		if seenPaths[m] || already[m] || n >= 3 {
			continue
		}
		seenPaths[m] = true
		data, err := readDeliverable(m)
		if err != nil || len(data) == 0 {
			continue
		}
		if sendByExt(chatID, m, data) {
			n++
		}
	}
}

// ---------------- incoming files ----------------

var unsafeName = regexp.MustCompile(`[^\w.\-]`)

type mediaInfo struct {
	Path string
	Kind string // image / video / audio / file
	Err  error
}

func storeIncoming(m Message) mediaInfo {
	var fileID, name, kind string
	stamp := time.Now().Unix()
	switch {
	case len(m.Photo) > 0:
		fileID, name, kind = m.Photo[len(m.Photo)-1].FileID, fmt.Sprintf("photo_%d.jpg", stamp), "image"
	case m.Video != nil:
		fileID, name, kind = m.Video.FileID, m.Video.FileName, "video"
		if name == "" {
			name = fmt.Sprintf("video_%d.mp4", stamp)
		}
	case m.VideoNote != nil:
		fileID, name, kind = m.VideoNote.FileID, fmt.Sprintf("note_%d.mp4", stamp), "video"
	case m.Animation != nil:
		fileID, name, kind = m.Animation.FileID, m.Animation.FileName, "video"
		if name == "" {
			name = fmt.Sprintf("anim_%d.mp4", stamp)
		}
	case m.Voice != nil:
		fileID, name, kind = m.Voice.FileID, fmt.Sprintf("voice_%d.ogg", stamp), "audio"
	case m.Audio != nil:
		fileID, name, kind = m.Audio.FileID, m.Audio.FileName, "audio"
		if name == "" {
			name = fmt.Sprintf("audio_%d.mp3", stamp)
		}
	case m.Document != nil:
		fileID, name, kind = m.Document.FileID, m.Document.FileName, "file"
		if name == "" {
			name = fmt.Sprintf("file_%d.bin", stamp)
		}
		switch mime := m.Document.MimeType; {
		case strings.HasPrefix(mime, "image/"):
			kind = "image"
		case strings.HasPrefix(mime, "audio/"):
			kind = "audio"
		case strings.HasPrefix(mime, "video/"):
			kind = "video"
		}
	}
	if fileID == "" {
		return mediaInfo{}
	}
	name = unsafeName.ReplaceAllString(filepath.Base(name), "_")
	name = fmt.Sprintf("%d_%d_%s", m.From.ID, time.Now().UnixNano(), name)
	data, err := downloadTGFile(tgFilePath(fileID))
	if err != nil {
		logf("下载失败: %v", err)
		return mediaInfo{Err: err}
	}
	if err := os.MkdirAll(uploadsDir, 0700); err != nil {
		return mediaInfo{Err: err}
	}
	full := filepath.Join(uploadsDir, name)
	if err := os.WriteFile(full, data, 0600); err != nil {
		logf("保存失败: %v", err)
		return mediaInfo{Err: err}
	}
	logf("收到 %s %s (%d B)", kind, full, len(data))
	return mediaInfo{Path: full, Kind: kind}
}

// transcribe calls an OpenAI-compatible /audio/transcriptions endpoint.
func transcribe(path string) (string, error) {
	c := configSnapshot()
	if c.ASR.BaseURL == "" || c.ASR.APIKey == "" {
		return "", fmt.Errorf("未配置语音识别")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	_, _ = fw.Write(data)
	_ = w.WriteField("model", c.ASR.Model)
	_ = w.Close()
	ctx, cancel := context.WithTimeout(telegramContext, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.ASR.BaseURL, "/")+"/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.ASR.APIKey)
	client := netClientContext(WithExecutionConfig(ctx, c))
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s", redactSecrets(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ASR HTTP %d: %s", resp.StatusCode, short(redactSecrets(string(raw)), 120))
	}
	var r struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	return r.Text, nil
}
