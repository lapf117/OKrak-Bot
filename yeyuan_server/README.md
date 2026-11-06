# 夜原 V5 中心服务器（V5.1 也用这个，不用更新）

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

## 换成你自己的服务器（V5.1 起）

1. 在你的服务器上用上面那条命令装好（想换端口：`curl -fsSL …/install.sh | PORT=50000 bash`）。
2. 手机上打开游戏：**联机 → 全球房间（服务器） → 右上角“改服务器”**，输入 `IP:端口`（例如 `1.2.3.4:47820`，不写端口就是 47820），点“保存”。房间列表会马上改连新服务器。
3. 点“默认”再“保存”可以恢复成内置的 154.12.20.154:47820。
4. 要一起玩的人必须连**同一个**服务器，才能看到彼此的房间。

地址存在游戏可写目录的 `server.txt` 里（一行 `IP:端口`）。

V5.1 和 V5.0 的地图不一样，两个版本不能进同一个房间（游戏会提示更新）；服务器程序本身没变。
