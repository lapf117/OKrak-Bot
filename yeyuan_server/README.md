# 夜原 V5 中心服务器

全球房间大厅 + 转发服务器。手机都连它：房主建房拿到六位房间号，别人在“联机 → 全球房间”里点房间或输入房间号加入。

- 协议：UDP 47820（ENet），和游戏用的是同一套通信
- 程序：`yeyuan_server`（静态链接的 Linux x86_64 程序，不依赖别的库）
- 游戏默认连的服务器：154.12.20.154:47820

## 安装（在服务器上用 root 运行一条命令）

```bash
curl -fsSL https://raw.githubusercontent.com/lapf117/okrak-bot/claude/yeyuan-v3-shots/yeyuan_server/install.sh | bash
```

脚本会：

1. 下载程序到 `/opt/yeyuan/`；
2. 装成 systemd 服务 `yeyuan`，开机自启、崩了自动重启；
3. 在 ufw / firewalld / iptables 里放行 UDP 47820。

**如果服务器在云厂商那里**，还要在控制台的“安全组 / 防火墙”里放行 **UDP 47820 入站**。

## 常用命令

```bash
systemctl status yeyuan      # 看状态
journalctl -u yeyuan -f      # 看日志（谁建了房、谁加入了）
systemctl restart yeyuan     # 重启
```
