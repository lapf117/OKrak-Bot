# 大肥鱼 Telegram Agent 2.1

> 2.1.1：纯文本界面（无表情），对话时无停止按钮（用 /stop 停止），普通用户只见使用、模型与推理选择，其余设置在管理员菜单与 config.json。

> English: a bilingual (中文/English) Telegram AI agent in pure Go. Send a task; it plans, calls tools with a stated intent for each step, shows live progress, and replies in formatted Markdown. Switch language with `/lang en`.

一个用 Go 标准库实现的 Telegram AI 工作助手。直接发送任务，即可搜索资料、读写代码、执行命令、维护计划并调用子智能体。主菜单保留常用入口，模型、执行策略、工作区、自动化与权限管理放在二级设置中。

本版参考并实际阅读了 [Codex CLI 官方源码](docs/CODEX_REFERENCE.md)，使用原生 Go 实现审批、取消、上下文压缩和技能机制。这里的执行权限是应用层控制；主机命令使用运行服务的系统用户权限。

## 快速开始

需要 Go 1.24+、bash 与 Linux 常用命令工具（包括 base64、mktemp、GNU chmod）；Git、SSH、ripgrep 按所需工具安装。构建无需下载第三方 Go 依赖。

```bash
go build -trimpath -o tg-agent .
cp config.example.json config.json
mkdir -p workspace
chmod 600 config.json
```

编辑 `config.json`：填入自己的 Telegram **用户 ID**（`owner_id`）、模型服务的 `base_url` 与真实模型名称。示例的 `YOUR_MODEL_NAME` 必须替换。设置凭据，然后验证与启动：

```bash
export TG_BOT_TOKEN='你的 BotFather token'
export LLM_API_KEY='你的模型 API key'
./tg-agent --config ./config.json --check
./tg-agent --config ./config.json
```

`--check` 只验证配置，不连接 Telegram 或模型。也支持 `--version`、`--help`。配置中的密钥支持 `env:变量名` 和 `${变量名}`；保存设置会保留环境变量引用。`TG_BOT_TOKEN` 可覆盖配置中的 token。不要提交实际配置或密钥。

此版只接受 **私聊**，用户 ID 必须在主人、管理员或朋友名单内。群聊会话不会启动任务。每个私聊同时运行一个任务；不同私聊可独立运行。任务启动时冻结模型、目录、执行模式与限制，更改设置从下一个任务生效。

## 菜单与操作

直接发送文字、图片、文件、视频或语音。语音转写需配置 `asr`；远端 SSH 工作区使用附件时，需要先把机器人主机的上传文件传到远端。

| 入口 | 用途 |
| --- | --- |
| `/menu` | 简洁任务面板 |
| `/new` | 新对话，不会自动终止运行中的任务 |
| `/stop` | 取消模型请求、正在执行的本地命令及等待的审批 |
| `/skills` | 本地技能、匹配与可信目录 |
| `/settings` | 模型、执行策略、工作区、自动化、权限与高级设置 |
| `/runs` | 最近执行记录，管理员可见 |
| `/continue` | 重新执行上一个任务；可能重复之前产生的操作 |
| `/file 路径` | 取回工作区、上传目录或执行记录内的文件，管理员可用 |
| `/health` | 模型、执行后端与资源自检，管理员可用 |

新增：`/lang zh|en|auto` 切换语言，`/memory` 长期备忘，`/export` 导出对话，`/steps on|off` 进度详情，`/status` 运行状态。

兼容原有 `/model`、`/provider`、`/effort`、`/system`、`/persona`、`/ws`、`/cmd`、`/cron`、`/admin`、`/friend`、`/ban`。机器人菜单只注册六个常用命令。`/think` 展示最近的可见答复摘要。

## 三种执行模式

通过“设置 → 执行策略”或 `/mode` 切换。

| 模式 | 行为 |
| --- | --- |
| `manual`（默认） | 任意 shell、文件修改、撤销、Git 写操作、技能安装及子任务启动前，发送一次性批准/拒绝按钮。子任务每次写操作继续审核。 |
| `auto` | 自动连续执行工具，受用户身份、任务预算、命令与任务超时限制。 |
| `read-only` | 服务端拒绝任意 shell、写文件、撤销、Git 写入、技能安装和子任务启动。 |

人工审批只由本次任务发起者在同一个私聊确认，批准仅适用于当前完整参数，重复点击和过期按钮失效。AI 不会替人点击批准。为保证审核内容完整可读，人工模式单次工具参数 JSON 超过 2200 字节会被拒绝；模型应拆分成较小的编辑。

朋友默认只有只读工具，路径限制在工作区内；模型伪造未列出的工具调用也会被服务端检查。`friend_tools: "all"` 会授予可信朋友包括任意 shell 和文件写入的主机操作权限，仍遵守执行策略；只读工具的路径检查不构成 shell 或写入沙箱。只读 Git 查询可以使用，Git 写操作需审批；文件和媒体回传限管理员。请把工作区放在数据目录之外或独立子目录中，避免在工作区存放凭据。

`auto` 能执行系统用户有权执行的命令。部署时建议使用专用普通用户、容器或独立工作主机；不会伪称提供 Codex 原生操作系统沙箱。SSH 连接关闭能结束客户端，但远端脱离会话的后台进程可能继续运行。

## 技能自动获取

本地自动发现：

```text
skills/code-task.md
skills/testing/SKILL.md
<workspace>/.skills/<name>.md
<workspace>/.agents/skills/<name>/SKILL.md
```

项目 `.agents/skills` 优先于 `.skills`，项目技能优先于全局同名技能。按技能名称、描述、触发条件和中英关键词为当前任务匹配最多三个技能；也支持模型用 `list_skills` / `load_skill` 主动加载。技能是辅助文本，不能覆盖权限和审批策略。自带编码、调研、运维、安全审查、发布检查及数据分析技能。

```markdown
---
name: testing
description: 测试与回归验证，test regression
when: 修改代码或修复 bug 后
---
先确定受影响行为，再运行最小必要验证，报告真实结果。
```

远程获取使用管理员配置的目录，而非运行网页里的安装脚本。将原始 Markdown URL、固定 SHA256 与主机白名单加入配置：

```json
{
  "auto_load_skills": true,
  "auto_fetch_skills": false,
  "skill_allowed_hosts": ["raw.githubusercontent.com"],
  "skill_sources": [
    {
      "name": "my-skill",
      "url": "https://raw.githubusercontent.com/YOUR_ORG/YOUR_REPO/COMMIT/skills/my-skill.md",
      "sha256": "替换为文件的64位SHA256十六进制摘要",
      "description": "代码测试、审查 test review"
    }
  ]
}
```

从审核过的文件运行 `sha256sum my-skill.md` 获取摘要；示例地址与摘要是占位值。下载必须使用 HTTPS、匹配精确主机白名单、通过 SHA256 校验，最多 128 KiB；重定向也会检查。安装只保存 Markdown 文本。

```text
/skills discover 测试
/skills install my-skill
/skills sync
/skills info my-skill
```

管理员手动发送 `install` / `sync` 命令就是本次安装授权；模型发起的 `install_skill` 仍受审批。只有同时配置 `auto_fetch_skills: true`、`auto_load_skills: true` 且当前为管理员的 `auto` 模式，才会按任务自动下载匹配的可信技能。首次启动不需要远程技能源。

SSH 后端只从机器人主机的 `skills_dir` 加载技能；需要使用远端项目技能时，请复制到机器人技能目录或通过可信目录分发。项目 `AGENTS.md` 在执行后端读取。

## 工具与任务可靠性

22 个工具（每次调用都需填写 intent 用途说明）：plan 计划、memory 记忆、schedule 定时、send_file 发送文件，以及命令、文件分页、精确编辑、项目地图、搜索、Git、撤销、任务清单、网络检测、网页检索、技能和子智能体。工具按依赖顺序执行。读写与撤销使用独立备份，备份失败会中止写入；精确编辑拒绝截断的原文。子任务继承用户、目录、执行策略、取消信号和总预算，不会提升为主人。

上下文按完整用户轮次裁剪，过长时压缩为交接摘要。私聊历史保存到 `sessions/`，重启后按需恢复。达到执行预算会返回未完成状态与下一步；错误、取消、成功都会记录。任务清单按任务隔离。

| 配置项 | 默认 |
| --- | --- |
| `max_steps` | 40 次模型调用预算，主任务与子任务共享 |
| `max_subagents` | 最多 4 个子任务；深度最多 3 层 |
| `command_timeout_seconds` | 120 秒 |
| `task_timeout_seconds` | 1800 秒 |
| `approval_timeout_seconds` | 300 秒 |
| `max_tool_output` | 24000 字节，超长结果截断并提示 |
| `max_history` | 30 轮 |
| `show_steps` | false，隐藏滚动详情，执行记录仍保存 |

任务到达预算时可另有一次不带工具的总结请求。工具输出截断后仍可能影响模型判断；应分页读取大文件。`read-only` 是应用层权限策略，不防御恶意项目里的可执行 Git 扩展或被攻陷的宿主工具；可信工作区仍是必要前提。

## 定时任务、快捷指令与 SSH

```text
/cron add daily 09:00 汇总服务器状态
/cron add weekdays 18:00 检查未提交改动
/cron add in 45m 提醒我开会
/cron pause 1 | resume 1 | run 1 | del 1
/cmd add review 检查当前项目的未提交改动并给出问题
/cmd review
/cron add 30m 检查服务状态并报告异常
/cron del 1
```

定时任务最短间隔 60 秒，使用创建者身份，并在触发前重新检查管理员权限。已有任务仍在运行时，不排队叠加同一私聊的新任务。旧版没有创建者 ID 的定时任务会停用，需要重新创建。

远程执行配置 `backend: "ssh"`、`ssh.host/user/port/key_path` 和远端绝对 `workspace`。先用服务用户执行一次正常 SSH 登录，核验主机指纹并写入 `known_hosts`；程序开启严格主机密钥校验，不再关闭校验。远端需有 bash、base64、mktemp 与 GNU coreutils；路径检查使用 `realpath`。

## 部署

推荐 Linux 普通用户。systemd 模板在 [deploy/tg-agent.service](deploy/tg-agent.service)，密钥环境文件放在 `/etc/tg-agent.env`，权限 `0600`，把服务用户和 `/opt/tg-agent` 目录权限设置好。模板将可写范围限制在该目录；需操作其他目录时按实际工作范围调整。

容器方式：

```bash
mkdir -p data/workspace
cp config.example.json data/config.json
cp -R skills data/skills
cp .env.example .env
# 编辑 .env 与 data/config.json
sudo chown -R 10001:10001 data
chmod 600 .env data/config.json
docker compose up -d --build
```

`data` 包含所有持久化文件；容器以 UID 10001 运行。备份实际配置、`cron.json`、`sessions/`、`runs/`、`skills/` 和工作区；备份含会话内容，应私密保存。执行日志默认不做自动轮转，长期运行请配置保留与清理策略。

## 验证与改动

```bash
go vet ./...
go test -race ./...
go build ./...
```

可用 `make release` 构建 Linux amd64 / arm64 单文件程序。本包的验证结果见 [docs/VALIDATION.md](docs/VALIDATION.md)，主要修复见 [CHANGELOG.md](CHANGELOG.md)，旧版迁移见 [升级说明](docs/UPGRADE.md)。没有实际 Telegram token / 模型 API key 的离线测试不会声称完成真实服务联调。

## 授权

本包没有替原始上传代码指定新的许可证。你需要确认对原代码、所选模型服务和第三方技能的使用与分发权利；不能仅凭本次改造保证所有商业使用权。[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 记录了 Codex 参考范围及上游许可信息。
