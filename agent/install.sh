#!/bin/sh
# CFSM Lite Agent 安装脚本。由 Worker 的 /install.sh 提供（已填入发布仓库），也可以直接保存后执行。
# 用法：
#   sh install.sh -i <节点ID> -s <上报密钥> -u <https://你的域名/update>
#   sh install.sh --uninstall
set -eu

REPO="__REPO__"
BIN=/opt/cfsm-lite/cfsm-lite-agent
ENV_DIR=/opt/cfsm-lite
ENV_FILE=/opt/cfsm-lite/agent.env
UNIT=/etc/systemd/system/cfsm-lite-agent.service

die() { echo "错误: $*" >&2; exit 1; }

ACTION=install
ID=""
SECRET=""
URL=""
TAG=latest
while [ $# -gt 0 ]; do
  case "$1" in
    -i) [ $# -ge 2 ] || die "-i 后面需要节点 ID"; ID="$2"; shift 2 ;;
    -s) [ $# -ge 2 ] || die "-s 后面需要上报密钥"; SECRET="$2"; shift 2 ;;
    -u) [ $# -ge 2 ] || die "-u 后面需要上报地址"; URL="$2"; shift 2 ;;
    -v) [ $# -ge 2 ] || die "-v 后面需要版本号，例如 v0.1.0"; TAG="$2"; shift 2 ;;
    --uninstall) ACTION=uninstall; shift ;;
    *) die "不认识的参数: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "需要 root 权限：请用 root 执行，或在 sh 前面加 sudo"
command -v systemctl >/dev/null 2>&1 || die "只支持使用 systemd 的 Linux 系统"

if [ "$ACTION" = uninstall ]; then
  systemctl disable --now cfsm-lite-agent >/dev/null 2>&1 || true
  rm -f "$UNIT" "$BIN"
  rm -rf "$ENV_DIR" /var/lib/private/cfsm-lite /var/lib/cfsm-lite
  systemctl daemon-reload
  echo "已卸载 cfsm-lite-agent"
  exit 0
fi

command -v curl >/dev/null 2>&1 || die "需要先安装 curl"
[ -n "$ID" ] && [ -n "$SECRET" ] && [ -n "$URL" ] || die "缺少参数：-i 节点ID  -s 上报密钥  -u 上报地址"
case "$URL" in https://*) ;; *) die "上报地址必须以 https:// 开头" ;; esac
case "$ID$SECRET$URL" in *[!A-Za-z0-9._:+/=?@-]*) die "节点 ID、密钥或地址里含有不支持的字符（只允许字母、数字和 . _ : + / = ? @ -）" ;; esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "不支持的 CPU 架构: $(uname -m)（只支持 x86_64 和 arm64）" ;;
esac

if [ "$TAG" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$TAG"
fi
NAME="cfsm-lite-agent-linux-$ARCH"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "下载 $NAME ..."
curl -fsSL "$BASE/$NAME" -o "$TMP/$NAME" || die "下载失败: $BASE/$NAME（请确认仓库 $REPO 已发布 Release）"
if command -v sha256sum >/dev/null 2>&1 && curl -fsSL "$BASE/SHA256SUMS" -o "$TMP/SHA256SUMS" 2>/dev/null; then
  (cd "$TMP" && grep " $NAME\$" SHA256SUMS | sha256sum -c - >/dev/null) || die "文件校验失败，可能下载损坏或被篡改，已中止"
  echo "校验通过"
else
  echo "警告: 无法校验文件完整性（缺少 sha256sum 或 SHA256SUMS）" >&2
fi

mkdir -p "$(dirname "$BIN")"
install -m 0755 "$TMP/$NAME" "$BIN.new"
mv -f "$BIN.new" "$BIN"

umask 077
mkdir -p "$ENV_DIR"
chmod 700 "$ENV_DIR"
cat > "$ENV_FILE" <<EOT
CFSM_URL=$URL
CFSM_ID=$ID
CFSM_SECRET=$SECRET
CFSM_STATE=/var/lib/cfsm-lite/state.json
EOT
chmod 600 "$ENV_FILE"

cat > "$UNIT" <<EOT
[Unit]
Description=CFSM Lite Agent
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=always
RestartSec=10
DynamicUser=yes
StateDirectory=cfsm-lite
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOT
chmod 644 "$UNIT"

systemctl daemon-reload
systemctl enable cfsm-lite-agent >/dev/null 2>&1
systemctl restart cfsm-lite-agent
sleep 3
if systemctl is-active --quiet cfsm-lite-agent; then
  echo "安装完成。查看运行日志: journalctl -u cfsm-lite-agent -f"
else
  echo "服务没有正常启动，请查看日志: journalctl -u cfsm-lite-agent -n 50" >&2
  exit 1
fi
