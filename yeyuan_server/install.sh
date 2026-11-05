#!/bin/bash
# 夜原 V5 中心服务器一键安装（Linux x86_64，需要 root；Ubuntu / Debian / CentOS 都行）
#
# 在服务器上运行（任选一种）：
#   1) 直接从 GitHub 下载安装：
#      curl -fsSL https://raw.githubusercontent.com/lapf117/okrak-bot/claude/yeyuan-v3-shots/yeyuan_server/install.sh | bash
#   2) 已经把 yeyuan_server 和这个脚本传到服务器同一个目录里：
#      bash install.sh
#
# 装好以后：服务名 yeyuan，开机自动启动，崩了自动重启；监听 UDP 47820。
#   看日志：journalctl -u yeyuan -f
#   重启：  systemctl restart yeyuan
#   停止：  systemctl stop yeyuan
set -e
PORT="${PORT:-47820}"
URL="https://raw.githubusercontent.com/lapf117/okrak-bot/claude/yeyuan-v3-shots/yeyuan_server/yeyuan_server"
DIR=/opt/yeyuan

if [ "$(id -u)" != "0" ]; then
  echo "请用 root 运行（或者在命令前加 sudo）"
  exit 1
fi

if [ "$(uname -m)" != "x86_64" ]; then
  echo "这个服务器程序是 x86_64 的，你的机器是 $(uname -m)。请用源码里的 server/build.sh 在服务器上自己编译。"
  exit 1
fi

mkdir -p "$DIR"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || echo .)"
if [ -f "$HERE/yeyuan_server" ]; then
  echo "使用当前目录的 yeyuan_server"
  install -m 755 "$HERE/yeyuan_server" "$DIR/yeyuan_server.new"
else
  echo "下载 yeyuan_server ……"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$URL" -o "$DIR/yeyuan_server.new"
  else
    wget -qO "$DIR/yeyuan_server.new" "$URL"
  fi
  chmod 755 "$DIR/yeyuan_server.new"
fi
mv -f "$DIR/yeyuan_server.new" "$DIR/yeyuan_server"

cat > /etc/systemd/system/yeyuan.service <<EOF
[Unit]
Description=YeYuan V5 lobby/relay server (UDP $PORT)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$DIR/yeyuan_server --port $PORT
Restart=always
RestartSec=2
DynamicUser=yes
NoNewPrivileges=yes
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable yeyuan >/dev/null 2>&1 || true
systemctl restart yeyuan

# 防火墙放行 UDP 端口（有哪个用哪个）
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
  ufw allow "$PORT"/udp >/dev/null && echo "ufw：已放行 UDP $PORT"
fi
if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
  firewall-cmd --permanent --add-port="$PORT"/udp >/dev/null && firewall-cmd --reload >/dev/null && echo "firewalld：已放行 UDP $PORT"
fi
if command -v iptables >/dev/null 2>&1; then
  iptables -C INPUT -p udp --dport "$PORT" -j ACCEPT 2>/dev/null || iptables -I INPUT -p udp --dport "$PORT" -j ACCEPT 2>/dev/null || true
fi

sleep 1
if systemctl is-active --quiet yeyuan; then
  echo "夜原服务器已经在运行：UDP $PORT"
  journalctl -u yeyuan -n 3 --no-pager 2>/dev/null || true
  echo "注意：如果服务器在云厂商那里（阿里云、腾讯云等），还要在控制台的“安全组 / 防火墙”里放行 UDP $PORT 入站。"
else
  echo "启动失败，看日志：journalctl -u yeyuan -n 50"
  exit 1
fi
