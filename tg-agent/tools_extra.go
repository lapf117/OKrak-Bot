package main

import (
	"context"
	"fmt"
	"strings"
)

// toolSendFileContext delivers a workspace file to the requesting chat.
func toolSendFileContext(ctx context.Context, args map[string]interface{}) (string, error) {
	chatID, _, _ := executionIdentity(ctx)
	if chatID == 0 {
		return "", fmt.Errorf("没有可发送的聊天会话")
	}
	if executionConfig(ctx).Backend == "ssh" {
		return "", fmt.Errorf("SSH 后端的文件位于远端主机，请先复制到机器人主机的工作区再发送")
	}
	path := strArg(args, "path")
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	full := absPathContext(ctx, path)
	data, err := readDeliverable(full)
	if err != nil {
		return "", err
	}
	caption := strings.TrimSpace(strArg(args, "caption"))
	if caption == "" {
		caption = full[strings.LastIndex(full, "/")+1:]
	}
	if !sendFileBytes(chatID, full, data, caption) {
		return "", fmt.Errorf("Telegram 发送失败")
	}
	markSent(ctx, full)
	return fmt.Sprintf("已发送 %s（%d 字节）", full, len(data)), nil
}

// toolScheduleContext lets the agent create and manage scheduled tasks for
// the current user. Jobs run later with the creator's identity and current
// execution settings; admins can review them with /cron.
func toolScheduleContext(ctx context.Context, args map[string]interface{}) (string, error) {
	chatID, uid, _ := executionIdentity(ctx)
	if uid == 0 || chatID == 0 {
		return "", fmt.Errorf("定时任务需要用户身份")
	}
	lang := ctxLang(ctx)
	switch action := strArg(args, "action"); action {
	case "list", "":
		if out := listCronText(lang, chatID); out != "" {
			return out, nil
		}
		return "（没有定时任务）", nil
	case "add":
		task := strings.TrimSpace(strArg(args, "task"))
		if task == "" {
			return "", fmt.Errorf("task 不能为空")
		}
		spec, err := parseScheduleText(strArg(args, "when"), scheduleLocation(executionConfig(ctx)))
		if err != nil {
			return "", err
		}
		job, err := addCronJob(chatID, uid, spec, task, "agent")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已创建定时任务 #%d：%s，下次 %s", job.ID, spec.describe(langZH), formatRunTime(job.NextRun)), nil
	case "cancel", "pause", "resume":
		id := intArg(args, "id", 0, 0, 1<<30)
		var err error
		switch action {
		case "cancel":
			err = deleteCronJob(id, chatID)
		case "pause":
			err = setCronEnabled(id, chatID, false)
		default:
			err = setCronEnabled(id, chatID, true)
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("定时任务 #%d 已%s", id, map[string]string{"cancel": "删除", "pause": "暂停", "resume": "恢复"}[action]), nil
	default:
		return "", fmt.Errorf("未知操作 %q，可用 add / list / cancel / pause / resume", action)
	}
}
