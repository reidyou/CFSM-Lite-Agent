#!/bin/sh
# CFSM Lite Agent 安装脚本。
#
# 用法一（推荐）：用 Worker 管理页「节点 → 安装命令」生成的命令。Worker 会把本脚本的发布仓库填好：
#   curl -fsSL https://你的域名/install.sh | sh -s -- -i 节点ID -s 上报密钥 -u https://你的域名/update
#
# 用法二：直接从 Agent 仓库执行，用环境变量 CFSM_AGENT_REPO 指定发布仓库：
#   curl -fsSL https://raw.githubusercontent.com/用户名/CFSM-Lite-Agent/main/install.sh | CFSM_AGENT_REPO=用户名/CFSM-Lite-Agent sh -s -- -i 节点ID -s 上报密钥 -u https://你的域名/update
#
# 卸载：在上面任意一条命令的 sh -s -- 后面改成 --uninstall
# 指定版本：加参数 -v v1.0.0（默认安装最新的 Release）
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

case "$REPO" in
  __*) REPO="$(printenv CFSM_AGENT_REPO || true)" ;; # 没有被 Worker 填好：改用环境变量
esac
[ -n "$REPO" ] || die "没有指定发布仓库：请设置环境变量 CFSM_AGENT_REPO，格式 用户名/CFSM-Lite-Agent"
case "$REPO" in
  */*) ;;
  *) die "发布仓库格式不对，应为 用户名/CFSM-Lite-Agent" ;;
esac
case "$REPO" in *[!A-Za-z0-9_./-]*) die "发布仓库里含有不支持的字符" ;; esac

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
chmod 755 "$(dirname "$BIN")"
install -m 0755 "$TMP/$NAME" "$BIN.new"
mv -f "$BIN.new" "$BIN"

umask 077
mkdir -p "$ENV_DIR"
chmod 755 "$ENV_DIR"
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
ProtectHome=read-only
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
