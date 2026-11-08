package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------- 定时任务 ----------------

type CronJob struct {
	ID      int    `json:"id"`
	ChatID  int64  `json:"chat_id"`
	UserID  int64  `json:"user_id"`
	Every   int64  `json:"every"` // 秒
	Prompt  string `json:"prompt"`
	NextRun int64  `json:"next_run"`
	Enabled bool   `json:"enabled"`
}

var (
	cronMu   sync.Mutex
	cronJobs []CronJob
	cronSeq  int
)

func cronPath() string { return filepath.Join(baseDir, "cron.json") }

func loadCron() {
	b, err := os.ReadFile(cronPath())
	if err != nil {
		return
	}
	cronMu.Lock()
	defer cronMu.Unlock()
	if err := json.Unmarshal(b, &cronJobs); err != nil {
		logf("定时任务配置损坏: %v", err)
		return
	}
	for i := range cronJobs {
		if cronJobs[i].UserID == 0 || cronJobs[i].Every < 60 {
			cronJobs[i].Enabled = false
		}
	}
	for _, j := range cronJobs {
		if j.ID > cronSeq {
			cronSeq = j.ID
		}
	}
}

func saveCron() {
	cronSaveMu.Lock()
	defer cronSaveMu.Unlock()
	cronMu.Lock()
	data, err := json.MarshalIndent(cronJobs, "", "  ")
	cronMu.Unlock()
	if err == nil {
		err = atomicWriteFile(cronPath(), data, 0600)
	}
	if err != nil {
		logf("定时任务保存失败: %v", err)
	}
}

var cronSaveMu sync.Mutex

func addCron(chatID int64, every int64, prompt string) int {
	return addCronFor(chatID, configSnapshot().OwnerID, every, prompt)
}
func addCronFor(chatID, uid int64, every int64, prompt string) int {
	if every < 60 || strings.TrimSpace(prompt) == "" {
		return 0
	}
	cronMu.Lock()
	cronSeq++
	id := cronSeq
	cronJobs = append(cronJobs, CronJob{ID: id, ChatID: chatID, UserID: uid, Every: every, Prompt: prompt, NextRun: time.Now().Unix() + every, Enabled: true})
	cronMu.Unlock()
	saveCron()
	return id
}

func delCron(id int) bool {
	cronMu.Lock()
	found := false
	var keep []CronJob
	for _, j := range cronJobs {
		if j.ID == id {
			found = true
			continue
		}
		keep = append(keep, j)
	}
	cronJobs = keep
	cronMu.Unlock()
	if found {
		saveCron()
	}
	return found
}

func listCron() string {
	cronMu.Lock()
	defer cronMu.Unlock()
	if len(cronJobs) == 0 {
		return "（没有定时任务）"
	}
	var b strings.Builder
	for _, j := range cronJobs {
		state := "启用"
		if !j.Enabled {
			state = "停用"
		}
		b.WriteString(fmt.Sprintf("#%d  %s  每 %s\n  %s\n  下次 %s\n",
			j.ID, state, humanEvery(j.Every), short(j.Prompt, 80),
			time.Unix(j.NextRun, 0).Format("01-02 15:04")))
	}
	return b.String()
}

func humanEvery(sec int64) string {
	switch {
	case sec%86400 == 0:
		return fmt.Sprintf("%d 天", sec/86400)
	case sec%3600 == 0:
		return fmt.Sprintf("%d 小时", sec/3600)
	case sec%60 == 0:
		return fmt.Sprintf("%d 分钟", sec/60)
	}
	return fmt.Sprintf("%d 秒", sec)
}

// startCron 后台调度
func startCron() {
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-telegramContext.Done():
				return
			case <-t.C:
			}
			now := time.Now().Unix()
			cronMu.Lock()
			var due []CronJob
			for i := range cronJobs {
				if cronJobs[i].Enabled && cronJobs[i].Every >= 60 && cronJobs[i].NextRun <= now {
					due = append(due, cronJobs[i])
					cronJobs[i].NextRun = now + cronJobs[i].Every
				}
			}
			cronMu.Unlock()
			if len(due) > 0 {
				saveCron()
			}
			for _, j := range due {
				job := j
				logf("定时任务 #%d 触发: %s", job.ID, short(job.Prompt, 60))
				go func() {
					if !allowed(job.UserID) || !isAdmin(job.UserID) {
						logf("定时任务 #%d 创建者已无管理员权限，跳过", job.ID)
						return
					}
					runUserTask(job.ChatID, job.UserID, job.Prompt, nil)
				}()
			}
		}
	}()
}

// ---------------- 快捷指令 ----------------

type QuickCmd struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

func findQuick(name string) (QuickCmd, bool) {
	cfg := configSnapshot()
	for _, q := range cfg.QuickCommands {
		if q.Name == name {
			return q, true
		}
	}
	return QuickCmd{}, false
}

func quickList() string {
	cfg := configSnapshot()
	if len(cfg.QuickCommands) == 0 {
		return "（还没有快捷指令）\n\n添加: /cmd add <名字> <提示词>"
	}
	var b strings.Builder
	for _, q := range cfg.QuickCommands {
		b.WriteString(fmt.Sprintf("- %s: %s\n", q.Name, short(q.Prompt, 70)))
	}
	return b.String()
}

// ---------------- 执行记录 ----------------

func listRuns(n int) string {
	dir := filepath.Join(baseDir, "runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "（还没有执行记录）"
	}
	type item struct {
		name string
		t    time.Time
	}
	var items []item
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			items = append(items, item{e.Name(), info.ModTime()})
		}
	}
	if len(items) == 0 {
		return "（还没有执行记录）"
	}
	// 按时间倒序
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].t.After(items[i].t) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	if n < 0 {
		n = 0
	}
	if n > len(items) {
		n = len(items)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("最近 %d 次执行\n\n", n))
	for _, it := range items[:n] {
		b.WriteString(fmt.Sprintf("%s  %s\n", it.t.Format("01-02 15:04"), it.name))
	}
	b.WriteString("\n取详情: /file " + filepath.Join(baseDir, "runs", "<文件名>"))
	return b.String()
}

// ---------------- 自检 ----------------

func healthCheck() string {
	cfg := configSnapshot()
	var b strings.Builder
	b.WriteString("自检\n")
	b.WriteString(strings.Repeat("─", 18) + "\n")

	// LLM
	start := time.Now()
	resp, err := chat([]LLMMessage{textMsg("user", "回答一个字：好")}, nil)
	if err != nil {
		b.WriteString(fmt.Sprintf("模型   失败: %s\n", short(err.Error(), 100)))
	} else {
		ans := short(strings.TrimSpace(textOf(resp.Choices[0].Message)), 20)
		b.WriteString(fmt.Sprintf("模型   正常 %.0fms（%s）\n", float64(time.Since(start).Milliseconds()), ans))
	}

	// 执行后端
	start = time.Now()
	out, err := runShell("echo backend_ok; pwd", 20*time.Second)
	if err != nil || !strings.Contains(out, "backend_ok") {
		b.WriteString(fmt.Sprintf("后端   失败: %s\n", short(fmt.Sprint(err, out), 100)))
	} else {
		b.WriteString(fmt.Sprintf("后端   正常 %.0fms（%s）\n",
			float64(time.Since(start).Milliseconds()), short(strings.TrimSpace(out), 60)))
	}

	// 资源
	if res, err := runShell("df -h / | tail -1 | awk '{print $5\" used\"}'; free -m | awk 'NR==2{print $3\"M/\"$2\"M mem\"}'", 20*time.Second); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(res), "\n") {
			b.WriteString("资源   " + strings.TrimSpace(line) + "\n")
		}
	}

	// 统计
	sk, _ := loadSkillIndex()
	b.WriteString(fmt.Sprintf("工具   %d 个\n", len(allTools())))
	b.WriteString(fmt.Sprintf("技能   %d 个\n", len(sk)))
	b.WriteString(fmt.Sprintf("模型   %s · 档位 %s\n", cfg.Model, cfg.ReasoningEffort))
	return b.String()
}
