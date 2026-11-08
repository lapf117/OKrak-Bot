package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// toolAccess classifies what a call can change. It decides approval and
// read-only enforcement on the server side, independent of the model.
type toolAccess int

const (
	accessRead  toolAccess = iota // no side effects outside this task
	accessSelf                    // changes only the user's own bot data (memory)
	accessWrite                   // changes files, processes, schedules or skills
)

type ToolDef struct {
	Name      string
	Desc      string
	Params    map[string]interface{}
	Run       func(context.Context, map[string]interface{}) (string, error)
	Read      bool                  // whole tool is read-only (friends may use it)
	Self      bool                  // whole tool only touches the user's own bot data
	Actions   map[string]toolAccess // per-action access for multi-action tools
	AdminOnly bool
}

// toolAliases keeps older tool names working.
var toolAliases = map[string]string{"todo": "plan"}

func canonicalToolName(name string) string {
	if alias, ok := toolAliases[name]; ok {
		return alias
	}
	return name
}

func obj(props map[string]interface{}, required ...string) map[string]interface{} {
	m := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func pStr(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func pNum(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "number", "description": desc}
}

func pBool(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

func pEnum(desc string, values ...string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "enum": values, "description": desc}
}

func pList(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": desc}
}

const intentDescription = "必填：一句话说明这一步做什么、为什么（用用户的语言）。Required: one short sentence on what this step does and why, in the user's language."

func allTools() []ToolDef {
	return []ToolDef{
		{Name: "plan", Read: true, Run: toolPlanContext,
			Desc: "维护本任务的执行计划，用户会实时看到。多步任务开始时用 set 列出步骤；开始某步 start，完成 done（不给 id 时作用于当前步骤）。Maintain the visible task plan.",
			Params: obj(map[string]interface{}{
				"action": pEnum("set / add / start / done / update / remove / list / clear", "set", "add", "start", "done", "update", "remove", "list", "clear"),
				"items":  pList("set 时的步骤列表，3–7 项为宜"),
				"text":   pStr("add / update 时的步骤描述"),
				"id":     pNum("步骤编号；start/done 省略时作用于当前步骤"),
			}, "action")},
		{Name: "bash", Run: toolBashContext,
			Desc: "在工作区执行 shell 命令，返回 stdout/stderr。Run a shell command in the workspace.",
			Params: obj(map[string]interface{}{
				"command": pStr("要执行的命令"),
				"timeout": pNum("超时秒数，默认 120，受管理员上限约束"),
			}, "command")},
		{Name: "read_file", Read: true, Run: toolReadFileContext,
			Desc: "读取文件内容；大文件用 offset/limit 分页，单次最多 2000 行；传目录则列出条目。Read a file page.",
			Params: obj(map[string]interface{}{
				"path":   pStr("文件或目录路径"),
				"offset": pNum("从第几行开始（1 起）"),
				"limit":  pNum("读多少行，默认 200"),
			}, "path")},
		{Name: "glob", Read: true, Run: toolGlobContext,
			Desc: "按文件名模式查找文件，如 *.go。Find files by name pattern.",
			Params: obj(map[string]interface{}{
				"pattern": pStr("文件名模式，如 *.py"),
				"path":    pStr("搜索目录，默认工作区"),
			}, "pattern")},
		{Name: "grep", Read: true, Run: toolGrepContext,
			Desc: "在目录里递归搜索文本内容（优先 ripgrep）。Search file contents.",
			Params: obj(map[string]interface{}{
				"pattern": pStr("正则或普通字符串"),
				"path":    pStr("搜索目录，默认工作区"),
				"include": pStr("只搜某类文件，如 *.go"),
			}, "pattern")},
		{Name: "list_dir", Read: true, Run: toolListDirContext,
			Desc:   "列出目录内容。List a directory.",
			Params: obj(map[string]interface{}{"path": pStr("目录路径")}, "path")},
		{Name: "repo_map", Read: true, Run: toolRepoMapContext,
			Desc: "生成项目结构地图：目录树、文件数、主要文件、语言分布。接手陌生项目先调用。Map a project.",
			Params: obj(map[string]interface{}{
				"path":  pStr("项目根目录，默认工作区"),
				"depth": pNum("目录深度，默认 3"),
			})},
		{Name: "write_file", Run: toolWriteFileContext,
			Desc: "写入整个文件（覆盖），自动建目录并备份。Write a whole file.",
			Params: obj(map[string]interface{}{
				"path": pStr("文件路径"), "content": pStr("完整内容"),
			}, "path", "content")},
		{Name: "edit_file", Run: toolEditFileContext,
			Desc: "精确替换文件中的一段文本；old_string 必须唯一匹配（或设 replace_all）。改代码优先用它。Exact text replacement.",
			Params: obj(map[string]interface{}{
				"path":        pStr("文件路径"),
				"old_string":  pStr("要被替换的原文本，需与文件完全一致"),
				"new_string":  pStr("替换后的新文本"),
				"replace_all": pBool("为 true 时替换全部匹配"),
			}, "path", "old_string", "new_string")},
		{Name: "undo", Run: toolUndoContext,
			Desc:   "撤销本任务最近的文件改动（write_file / edit_file 自动备份）。Undo recent file edits.",
			Params: obj(map[string]interface{}{"count": pNum("撤销几步，默认 1，最多 5")})},
		{Name: "git", Run: toolGitContext,
			Actions: map[string]toolAccess{"status": accessRead, "diff": accessRead, "log": accessRead, "branches": accessRead, "show": accessRead, "add": accessWrite, "commit": accessWrite},
			Desc:    "Git 操作：status / diff / log / branches / show 为只读；add / commit 需要写权限。Git queries and commits.",
			Params: obj(map[string]interface{}{
				"action":  pEnum("Git 操作", "status", "diff", "log", "branches", "show", "add", "commit"),
				"message": pStr("commit 时的提交信息"),
				"path":    pStr("仓库目录，默认工作区"),
			}, "action")},
		{Name: "web_search", Read: true, Run: toolWebSearchContext,
			Desc: "联网搜索，返回标题、链接与摘要。Web search with snippets.",
			Params: obj(map[string]interface{}{
				"query": pStr("搜索词"),
				"count": pNum("结果数，默认 8，最多 15"),
			}, "query")},
		{Name: "web_fetch", Read: true, Run: toolWebFetchContext,
			Desc:   "抓取网页并转成保留段落的纯文本。Fetch a web page as text.",
			Params: obj(map[string]interface{}{"url": pStr("http/https URL")}, "url")},
		{Name: "net_check", Read: true, Run: toolNetCheckContext,
			Desc:   "检测网络连通性、DNS 和代理。联网失败时先运行。Diagnose connectivity.",
			Params: obj(map[string]interface{}{})},
		{Name: "memory", Run: toolMemoryContext,
			Actions: map[string]toolAccess{"list": accessRead, "add": accessSelf, "delete": accessSelf},
			Desc:    "用户的长期备忘：add 保存用户明确希望记住的偏好/事实，list 查看，delete 删除。不要保存密钥。Long-term user notes.",
			Params: obj(map[string]interface{}{
				"action": pEnum("add / list / delete", "add", "list", "delete"),
				"text":   pStr("add 时要记住的内容，一条一事"),
				"id":     pNum("delete 时的编号"),
			}, "action")},
		{Name: "schedule", Run: toolScheduleContext, AdminOnly: true,
			Actions: map[string]toolAccess{"list": accessRead, "add": accessWrite, "cancel": accessWrite, "pause": accessWrite, "resume": accessWrite},
			Desc:    "为用户创建或管理定时任务（到时以用户身份自动执行 task）。when 写法：every 30m · daily 09:00 · weekdays 09:00 · at 2026-12-01 09:00 · at 18:30 · in 2h。Schedule future or recurring tasks.",
			Params: obj(map[string]interface{}{
				"action": pEnum("add / list / cancel / pause / resume", "add", "list", "cancel", "pause", "resume"),
				"when":   pStr("add 时的时间规则，例如 daily 09:00 或 in 30m"),
				"task":   pStr("add 时到点要执行的完整任务描述"),
				"id":     pNum("cancel / pause / resume 时的定时任务编号"),
			}, "action")},
		{Name: "send_file", Read: true, AdminOnly: true, Run: toolSendFileContext,
			Desc: "把工作区中的文件（报告、图片、压缩包等）直接发送给用户。Send a workspace file to the user.",
			Params: obj(map[string]interface{}{
				"path":    pStr("文件路径"),
				"caption": pStr("可选说明"),
			}, "path")},
		{Name: "list_skills", Read: true, Run: toolListSkillsContext,
			Desc:   "列出可加载的技能。List skills.",
			Params: obj(map[string]interface{}{})},
		{Name: "load_skill", Read: true, Run: toolLoadSkillContext,
			Desc:   "加载指定技能的完整说明。Load a skill.",
			Params: obj(map[string]interface{}{"name": pStr("技能名")}, "name")},
		{Name: "discover_skills", Read: true, Run: toolDiscoverSkillsContext,
			Desc:   "搜索管理员配置的可信技能目录。Search the trusted skill catalogue.",
			Params: obj(map[string]interface{}{"query": pStr("技能关键词，可空")})},
		{Name: "install_skill", AdminOnly: true, Run: toolInstallSkillContext,
			Desc:   "从管理员配置的可信来源安装文本技能（固定 SHA256）。Install a pinned skill.",
			Params: obj(map[string]interface{}{"name": pStr("可信目录中的技能名")}, "name")},
		{Name: "spawn_agent", Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
			return spawnAgentContext(ctx, strArg(args, "task"))
		},
			Desc:   "派出一个子智能体独立完成子任务并返回结论；适合拆分复杂工作。Delegate a sub-task.",
			Params: obj(map[string]interface{}{"task": pStr("交给子智能体的完整任务说明")}, "task")},
	}
}

func findTool(name string) (ToolDef, bool) {
	name = canonicalToolName(name)
	for _, t := range allTools() {
		if t.Name == name {
			return t, true
		}
	}
	return ToolDef{}, false
}

func (t ToolDef) baseAccess() toolAccess {
	switch {
	case t.Read:
		return accessRead
	case t.Self:
		return accessSelf
	}
	return accessWrite
}

// classifyTool decides the access class of one concrete call.
func classifyTool(name, argsJSON string) toolAccess {
	t, ok := findTool(name)
	if !ok {
		return accessWrite
	}
	if t.Actions == nil {
		return t.baseAccess()
	}
	var args struct {
		Action string `json:"action"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return accessWrite
	}
	if access, ok := t.Actions[args.Action]; ok {
		return access
	}
	return accessWrite
}

func toolReadOnly(name, argsJSON string) bool { return classifyTool(name, argsJSON) == accessRead }

// ---------------- schemas ----------------

type toolPolicy struct {
	ReadOnlyMode   bool // execution mode is read-only
	FriendReadOnly bool // a friend without friend_tools=all
	Admin          bool
}

func (p toolPolicy) permits(access toolAccess) bool {
	switch access {
	case accessRead:
		return true
	case accessSelf:
		return !p.ReadOnlyMode
	}
	return !p.ReadOnlyMode && !p.FriendReadOnly
}

func policyFor(ctx context.Context, uid int64) toolPolicy {
	c := executionConfig(ctx)
	admin := isAdmin(uid)
	return toolPolicy{ReadOnlyMode: c.ApprovalMode == "read-only", FriendReadOnly: !admin && c.FriendTools != "all", Admin: admin}
}

// withIntent adds the intent parameter every call should explain itself with.
func withIntent(params map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range params {
		out[k] = v
	}
	props := map[string]interface{}{"intent": pStr(intentDescription)}
	if old, ok := params["properties"].(map[string]interface{}); ok {
		for k, v := range old {
			props[k] = v
		}
	}
	out["properties"] = props
	required := []string{"intent"}
	if old, ok := params["required"].([]string); ok {
		required = append(old, "intent")
	}
	out["required"] = required
	return out
}

func schemasForPolicy(p toolPolicy) []ToolSchema {
	var out []ToolSchema
	for _, t := range allTools() {
		if t.AdminOnly && !p.Admin {
			continue
		}
		params := t.Params
		if t.Actions != nil {
			var allowedActions []string
			props := params["properties"].(map[string]interface{})
			action := props["action"].(map[string]interface{})
			for _, a := range action["enum"].([]string) {
				if p.permits(t.Actions[a]) {
					allowedActions = append(allowedActions, a)
				}
			}
			if len(allowedActions) == 0 {
				continue
			}
			if len(allowedActions) < len(t.Actions) {
				props["action"] = pEnum("可用操作（受当前权限限制）", allowedActions...)
				if t.Name == "git" {
					t.Desc = "只读 Git 查询：" + strings.Join(allowedActions, " / ") + "。Read-only git queries."
				}
			}
		} else if !p.permits(t.baseAccess()) {
			continue
		}
		var schema ToolSchema
		schema.Type = "function"
		schema.Function.Name = t.Name
		schema.Function.Description = t.Desc
		schema.Function.Parameters = withIntent(params)
		out = append(out, schema)
	}
	return out
}

func toolSchemas(forFriend bool) []ToolSchema {
	return schemasForPolicy(toolPolicy{FriendReadOnly: forFriend, Admin: !forFriend})
}

func toolSchemasContext(ctx context.Context, uid int64) []ToolSchema {
	return schemasForPolicy(policyFor(ctx, uid))
}

func toolSchemasFor(uid int64) []ToolSchema { return toolSchemasContext(context.Background(), uid) }

// ---------------- dispatch ----------------

func runTool(name, argsJSON string) (string, error) {
	return runToolContext(context.Background(), name, argsJSON)
}

// runToolContext validates, authorizes and runs one tool call.
func runToolContext(ctx context.Context, name, argsJSON string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(argsJSON) > 4<<20 {
		return "", fmt.Errorf("工具参数过大")
	}
	var args map[string]interface{}
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("参数不是 JSON 对象: %w", err)
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	delete(args, "intent")
	t, ok := findTool(name)
	if !ok {
		return "", fmt.Errorf("没有这个工具: %s", name)
	}
	if t.Run == nil {
		return "", fmt.Errorf("工具未绑定")
	}
	if err := authorizeTool(ctx, t.Name, argsJSON); err != nil {
		return "", err
	}
	if err := validateToolPathContext(ctx, t.Name, args); err != nil {
		return "", err
	}
	out, err := t.Run(ctx, args)
	if limit := outputLimit(ctx); len(out) > limit {
		out = strings.ToValidUTF8(out[:limit], "�") + "\n…（输出过长已截断）"
	}
	return out, err
}

func pathInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func pathInsideTool(root, target string) bool { return pathInside(root, target) }

// validateToolPathContext is an application permission boundary for friends,
// not an OS sandbox.
func validateToolPathContext(ctx context.Context, name string, args map[string]interface{}) error {
	if isAdmin(executionUID(ctx)) {
		return nil
	}
	switch name {
	case "read_file", "grep", "glob", "list_dir", "repo_map", "git":
	default:
		return nil
	}
	c := executionConfig(ctx)
	full := absPathContext(ctx, strArg(args, "path"))
	root := filepath.Clean(c.Workspace)
	var err error
	if c.Backend == "ssh" {
		if full, err = runShellContext(ctx, "realpath -e -- "+shellQuote(full), 10*time.Second); err != nil {
			return fmt.Errorf("无法确认工作区路径: %w", err)
		}
		if root, err = runShellContext(ctx, "realpath -e -- "+shellQuote(root), 10*time.Second); err != nil {
			return fmt.Errorf("无法确认工作区: %w", err)
		}
		full, root = strings.TrimSpace(full), strings.TrimSpace(root)
	} else {
		if full, err = filepath.EvalSymlinks(full); err != nil {
			return err
		}
		if root, err = filepath.EvalSymlinks(root); err != nil {
			return err
		}
	}
	if !pathInside(root, full) {
		return fmt.Errorf("朋友只允许读取工作区内的路径")
	}
	if c.Backend != "ssh" && baseDir != "" {
		protected := []string{configPath(), filepath.Join(baseDir, "sessions"), filepath.Join(baseDir, "runs"), filepath.Join(baseDir, "agent.log"), filepath.Join(baseDir, "cron.json"), filepath.Join(baseDir, "memory"), filepath.Join(baseDir, "prefs.json")}
		for _, secret := range protected {
			secret, _ = filepath.Abs(secret)
			if canonical, e := filepath.EvalSymlinks(secret); e == nil {
				secret = canonical
			}
			if pathInside(secret, full) || (name != "read_file" && name != "list_dir" && pathInside(full, secret)) {
				return fmt.Errorf("禁止读取机器人配置和其他会话数据，请将工作区与数据目录分开")
			}
		}
	}
	// Prevent a final symlink swap for ordinary reads by replacing it with the canonical path.
	args["path"] = full
	return nil
}
