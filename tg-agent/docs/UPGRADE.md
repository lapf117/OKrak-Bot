# 从原版升级

1. 停止旧机器人进程，备份配置、技能、定时任务和工作区。Telegram 同一 token 只运行一个轮询进程。
2. 使用 Go 1.24+ 构建，替换程序。保留原配置；需要新配置时参照根目录 config.example.json。
3. 确认 owner_id 为自己的正整数用户 ID，当前模型与 API key 已配置；运行 `tg-agent --config /绝对路径/config.json --check`。
4. 将 workspace 设为专用工作目录，避免直接使用包含机器人配置和历史的目录。给服务普通用户相应权限。
5. 启动后在机器人私聊打开 `/settings`。默认执行模式为 manual，写操作等待一次性审批；如需连续执行，用 `/mode auto` 明确切换。

## 行为变化

- 只允许私聊；群组不再执行任务，避免不同用户共享历史与结果。
- 朋友默认只能使用工作区范围内的只读工具，文件回传需要管理员。`friend_tools: all` 是可信用户的主机命令权限。
- `/stop` 取消当前任务；`/continue` 重新执行上次任务，可能重复写入或外部操作。
- 新版子任务不再拥有主人的身份，调用预算、取消和审批均继承父任务。
- 旧的无 user_id 定时任务会保留并停用。用 `/cron` 查看，删除后重新创建，记录实际创建者。
- SSH 不再忽略主机指纹。请以运行机器人的服务用户先核验并保存 known_hosts。
- 撤销历史只在当前任务中共享；每个版本有独立 .agentbak.* 文件。主机备份不等同于跨任务自动撤销索引。
- 配置、会话和新日志采用私密文件权限；旧文件权限不会强制更改，应在迁移时检查。
- `show_steps` 可隐藏滚动工具细节；详情仍保存到 runs/，失败和取消也会记录。

## 数据与故障排查

数据目录是配置文件所在目录：config.json、agent.log、cron.json、updates.offset、sessions/、runs/、uploads/。相对 workspace/skills_dir 也以该目录解析。机器人读写技能在本地主机；SSH 工作区文件位于远端，不会自动镜像附件或技能。

模型报参数不支持时，先将 reasoning_effort 设为空；填写该项时需使用服务商支持的值。每个 provider 使用 OpenAI 兼容的 Chat Completions 接口，不自动转换其他协议。

如使用 systemd，请将服务用户、可写目录和 EnvironmentFile 按部署实际位置修改。启动失败返回非零退出码，模板可自动重启。

## 2.3 → 2.4

- 管理入口改为服务器终端：`sudo -u tgagent /opt/tg-agent/tg-agent --config /opt/tg-agent/config.json admin`（容器：`docker compose exec agent tg-agent --config /app/config.json admin`）。Telegram 中的 `/admin` 面板与管理命令已移除。
- 机器人运行时会在数据目录创建 `admin.sock`（权限 0600），退出时删除；它是终端与机器人通信用的，不要放进备份或版本库。
- 配置项没有变化。

## 2.2 → 2.3

- 新增可选配置：`registration`（默认 `approval`）、`bot_name`、`author`、`support_text`、`support_url`。**行为变化**：默认允许陌生人提交注册申请，管理员会收到审核通知；如需保持只能按 ID 添加，设置 `"registration": "off"`。
- 设置菜单只剩模型、执行模式、推理；其余设置在 `/admin` 管理后台，原有命令不变。旧消息上的“高级”按钮会打开管理后台。
- 统计行默认不显示余额；已在 /show 中手动开启的用户保持原选择。
- 新增运行数据文件 `registrations.json`、`announcements.json`，请与其他运行数据一起备份。

## 2.1 → 2.2

- 直接替换程序即可，原有 `config.json`、`prefs.json` 无需修改；新增配置项均为可选：`display`（显示开关的全员默认值）与 `llm_retries`（默认 6）。`display` 中写了未知项会在启动和 `--check` 时报错。
- 回复下方不再有按钮。旧消息上的“新对话 / 重试 / 菜单 / 执行记录”按钮仍可点击；执行记录改用 `/runs` 获取。
- 用户的显示选择写入 `prefs.json` 的 `show` 字段；`/steps` 与 `show_steps` 继续控制“输出预览”。
- 模型接口失败时任务会先在后台重试（默认最长约 1 分钟）再报错，长时间无响应的服务商可调低 `llm_retries`。
