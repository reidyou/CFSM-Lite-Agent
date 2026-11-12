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
# 支持 systemd（Debian、Ubuntu、Armbian 等）和 OpenRC（Alpine）。Alpine 会自动补装 curl、ca-certificates、tzdata。
# 指定版本：加参数 -v v1.0.0（默认安装最新的 Release）
# 完整性校验：默认必须通过 SHA256SUMS 校验，拿不到校验文件或没有 sha256sum 时会中止；确实需要跳过时加参数 --skip-verify
set -eu

REPO="__REPO__"
BIN=/opt/cfsm-lite/cfsm-lite-agent
ENV_DIR=/opt/cfsm-lite
ENV_FILE=/opt/cfsm-lite/agent.env
UNIT=/etc/systemd/system/cfsm-lite-agent.service
RC_FILE=/etc/init.d/cfsm-lite-agent
RC_LOG=/var/log/cfsm-lite-agent.log

die() { echo "错误: $*" >&2; exit 1; }

ACTION=install
ID=""
SECRET=""
URL=""
TAG=latest
SKIP_VERIFY=0
while [ $# -gt 0 ]; do
  case "$1" in
    -i) [ $# -ge 2 ] || die "-i 后面需要节点 ID"; ID="$2"; shift 2 ;;
    -s) [ $# -ge 2 ] || die "-s 后面需要上报密钥"; SECRET="$2"; shift 2 ;;
    -u) [ $# -ge 2 ] || die "-u 后面需要上报地址"; URL="$2"; shift 2 ;;
    -v) [ $# -ge 2 ] || die "-v 后面需要版本号，例如 v0.1.0"; TAG="$2"; shift 2 ;;
    --uninstall) ACTION=uninstall; shift ;;
    --skip-verify) SKIP_VERIFY=1; shift ;;
    *) die "不认识的参数: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "需要 root 权限：请用 root 执行，或在 sh 前面加 sudo"

# 识别发行版和服务管理器：systemd（Debian、Ubuntu、Armbian、CentOS 等）或 OpenRC（Alpine）
DISTRO=linux
if [ -f /etc/alpine-release ]; then DISTRO=alpine
elif [ -f /etc/armbian-release ]; then DISTRO=armbian
fi
INIT=""
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then INIT=systemd
elif command -v rc-service >/dev/null 2>&1 && command -v rc-update >/dev/null 2>&1; then INIT=openrc
fi

if [ "$ACTION" = uninstall ]; then
  # 不管当初装的是哪种服务，两种都清理
  if command -v systemctl >/dev/null 2>&1; then systemctl disable --now cfsm-lite-agent >/dev/null 2>&1 || true; fi
  if [ -f "$RC_FILE" ] && command -v rc-service >/dev/null 2>&1; then
    rc-service cfsm-lite-agent stop >/dev/null 2>&1 || true
    rc-update del cfsm-lite-agent default >/dev/null 2>&1 || true
  fi
  rm -f "$UNIT" "$RC_FILE" "$RC_LOG" "$BIN"
  rm -rf "$ENV_DIR"
  if command -v userdel >/dev/null 2>&1; then userdel cfsm-lite >/dev/null 2>&1 || true
  elif command -v deluser >/dev/null 2>&1; then deluser cfsm-lite >/dev/null 2>&1 || true
  fi
  if command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload >/dev/null 2>&1 || true; fi
  echo "已卸载 cfsm-lite-agent"
  exit 0
fi

[ -n "$INIT" ] || die "只支持使用 systemd 或 OpenRC 的 Linux 系统（Debian、Ubuntu、Armbian、Alpine 等）"

case "$REPO" in
  __*) REPO="$(printenv CFSM_AGENT_REPO || true)" ;; # 没有被 Worker 填好：改用环境变量
esac
[ -n "$REPO" ] || die "没有指定发布仓库：请设置环境变量 CFSM_AGENT_REPO，格式 用户名/CFSM-Lite-Agent"
case "$REPO" in
  */*) ;;
  *) die "发布仓库格式不对，应为 用户名/CFSM-Lite-Agent" ;;
esac
case "$REPO" in *[!A-Za-z0-9_./-]*) die "发布仓库里含有不支持的字符" ;; esac

# Alpine 默认没有 curl、CA 证书和时区数据库：缺什么补什么（Agent 访问 https 和换算时区都要用到）
if [ "$DISTRO" = alpine ] && command -v apk >/dev/null 2>&1; then
  NEED=""
  command -v curl >/dev/null 2>&1 || NEED="$NEED curl"
  [ -f /etc/ssl/certs/ca-certificates.crt ] || NEED="$NEED ca-certificates"
  [ -d /usr/share/zoneinfo ] || NEED="$NEED tzdata"
  if [ -n "$NEED" ]; then
    echo "安装依赖:$NEED"
    # shellcheck disable=SC2086
    apk add --no-cache $NEED >/dev/null || die "安装依赖失败:$NEED"
  fi
fi
command -v curl >/dev/null 2>&1 || die "需要先安装 curl"
[ -n "$ID" ] && [ -n "$SECRET" ] && [ -n "$URL" ] || die "缺少参数：-i 节点ID  -s 上报密钥  -u 上报地址"
case "$URL" in https://*) ;; *) die "上报地址必须以 https:// 开头" ;; esac
case "$ID$SECRET$URL" in *[!A-Za-z0-9._:+/=?@-]*) die "节点 ID、密钥或地址里含有不支持的字符（只允许字母、数字和 . _ : + / = ? @ -）" ;; esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "不支持的 CPU 架构: $(uname -m)（支持 x86_64 和 arm64）" ;;
esac

if [ "$TAG" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$TAG"
fi
NAME="cfsm-lite-agent-linux-$ARCH"
# 只走 HTTPS（重定向也一样）、至少 TLS 1.2；网络抖动时自动重试，连接和总耗时都有上限，不会一直卡住
CURL_OPTS="--proto =https --tlsv1.2 --retry 3 --retry-delay 2 --connect-timeout 10"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "下载 $NAME ..."
curl -fsSL $CURL_OPTS --max-time 300 "$BASE/$NAME" -o "$TMP/$NAME" || die "下载失败: $BASE/$NAME（请确认仓库 $REPO 已发布 Release，并且包含 $ARCH 版本）"
GOT="$NAME"
# 完整性校验默认必须通过：缺少 sha256sum、下载不到 SHA256SUMS、或校验不一致，都中止安装（不会装一个没校验过的程序）
if [ "$SKIP_VERIFY" = 1 ]; then
  echo "警告: 已按 --skip-verify 跳过完整性校验" >&2
else
  command -v sha256sum >/dev/null 2>&1 || die "缺少 sha256sum，无法校验文件完整性（请先安装 coreutils，或确认风险后加参数 --skip-verify 跳过校验）"
  curl -fsSL $CURL_OPTS --max-time 60 "$BASE/SHA256SUMS" -o "$TMP/SHA256SUMS" 2>/dev/null || die "下载校验文件失败: $BASE/SHA256SUMS（请确认 Release 里有 SHA256SUMS；确认风险后可加参数 --skip-verify 跳过校验）"
  (cd "$TMP" && grep " $GOT\$" SHA256SUMS | sha256sum -c - >/dev/null) || die "文件校验失败，可能下载损坏或被篡改，已中止"
  echo "校验通过"
fi

mkdir -p "$(dirname "$BIN")"
chmod 755 "$(dirname "$BIN")"
install -m 0755 "$TMP/$GOT" "$BIN.new"
# 先确认二进制能在本机运行：Agent 是静态编译的，Alpine（musl）可以直接用；万一装了动态链接 glibc 的程序时，执行会直接失败（退出码 126 / 127）。
# 空环境里最多跑 3 秒：程序配置不全会自己退出，超时被杀（124）或正常退出都算通过，只有「无法执行」才中止。
if command -v timeout >/dev/null 2>&1; then
  RC=0
  env -i CFSM_STATE=/nonexistent/state.json timeout 3 "$BIN.new" >/dev/null 2>&1 || RC=$?
  if [ "$RC" -eq 126 ] || [ "$RC" -eq 127 ]; then
    rm -f "$BIN.new"
    die "下载的程序无法在本机运行（退出码 $RC）。Alpine 等 musl 系统需要静态编译的 Agent（CGO_ENABLED=0）"
  fi
fi
mv -f "$BIN.new" "$BIN"

# 状态文件放在程序同一目录（$ENV_DIR/state.json）。服务用专门的系统用户运行，只有这个目录归它所有；
# 程序文件和 agent.env 仍归 root。写状态文件时要在同一目录里建临时文件再改名，所以目录必须可写。
if ! id cfsm-lite >/dev/null 2>&1; then
  NOLOGIN=/bin/false
  for sh in /usr/sbin/nologin /sbin/nologin; do [ -x "$sh" ] && { NOLOGIN="$sh"; break; }; done
  if command -v useradd >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir /nonexistent --shell "$NOLOGIN" cfsm-lite \
      || die "创建服务用户 cfsm-lite 失败"
  elif command -v adduser >/dev/null 2>&1; then # Alpine（BusyBox）
    adduser -S -D -H -h /nonexistent -s "$NOLOGIN" cfsm-lite \
      || die "创建服务用户 cfsm-lite 失败"
  else
    die "需要 useradd 或 adduser 命令来创建服务用户 cfsm-lite"
  fi
fi

umask 077
mkdir -p "$ENV_DIR"
chmod 755 "$ENV_DIR"
chown cfsm-lite "$ENV_DIR"
STATE="$ENV_DIR/state.json"
[ ! -f "$STATE" ] || { chown cfsm-lite "$STATE"; chmod 600 "$STATE"; }
cat > "$ENV_FILE" <<EOT
CFSM_URL=$URL
CFSM_ID=$ID
CFSM_SECRET=$SECRET
CFSM_STATE=$STATE
EOT
chmod 600 "$ENV_FILE"

if [ "$INIT" = systemd ]; then
  # 写服务文件：基础加固项一直保留；扩展加固项（full）只收紧 Agent 用不到的能力：
  # 网络只留 IPv4、IPv6 和本地套接字，内核参数、模块、日志、控制组、系统时钟、主机名一律只读或禁止，隔离设备，
  # 限制内存和线程数。刻意没有加 ProtectProc=invisible：它会让进程数统计只剩 Agent 自己的进程。
  write_unit() {
    {
      cat <<EOT
[Unit]
Description=CFSM Lite Agent
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=always
RestartSec=10
User=cfsm-lite
ReadWritePaths=$ENV_DIR
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
CapabilityBoundingSet=
EOT
      if [ "$1" = full ]; then
        cat <<'EOT'
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
SystemCallArchitectures=native
UMask=0077
MemoryMax=128M
TasksMax=128
EOT
      fi
      cat <<'EOT'

[Install]
WantedBy=multi-user.target
EOT
    } > "$UNIT"
    chmod 644 "$UNIT"
  }

  write_unit full
  systemctl daemon-reload
  systemctl enable cfsm-lite-agent >/dev/null 2>&1
  systemctl restart cfsm-lite-agent || true
  sleep 3
  if ! systemctl is-active --quiet cfsm-lite-agent; then
    # 个别系统（很旧的 systemd、特殊内核）可能不接受扩展加固项：退回基础加固再试一次，保证 Agent 能运行
    echo "警告: 启用扩展加固后服务没有正常启动，改用基础加固重试" >&2
    write_unit basic
    systemctl daemon-reload
    systemctl restart cfsm-lite-agent || true
    sleep 3
  fi
  if systemctl is-active --quiet cfsm-lite-agent; then
    echo "安装完成。查看运行日志: journalctl -u cfsm-lite-agent -f"
  else
    echo "服务没有正常启动，请查看日志: journalctl -u cfsm-lite-agent -n 50" >&2
    exit 1
  fi
else
  # OpenRC（Alpine）：用 supervise-daemon 守护，异常退出 10 秒后自动重启，和 systemd 的 Restart=always 一致
  cat > "$RC_FILE" <<EOT
#!/sbin/openrc-run
name="cfsm-lite-agent"
description="CFSM Lite Agent"
command="$BIN"
command_user="cfsm-lite"
supervisor="supervise-daemon"
respawn_delay=10
respawn_max=0
output_log="$RC_LOG"
error_log="$RC_LOG"

depend() {
	need net
	after firewall
}

start_pre() {
	# supervise-daemon 会先切换到 command_user，再打开日志文件；/var/log 只有 root 能写，
	# 日志文件必须事先建好并归 cfsm-lite 所有，否则子进程每次都在打开日志时退出，服务显示 started 却没有任何输出和上报
	checkpath -f -o cfsm-lite -m 0640 "\$output_log" || return 1
	# 日志超过 5MB 就清空，避免占满小容量的 Alpine 磁盘
	if [ -f "\$output_log" ] && [ "\$(wc -c < "\$output_log")" -gt 5242880 ]; then : > "\$output_log"; fi
	set -a
	. "$ENV_FILE"
	set +a
}
EOT
  chmod 755 "$RC_FILE"
  # 提前建好日志文件并交给服务用户（start_pre 里的 checkpath 会再保证一次，日志被删除后也能自动重建）
  : >> "$RC_LOG"
  chown cfsm-lite "$RC_LOG"
  chmod 640 "$RC_LOG"
  rc-update add cfsm-lite-agent default >/dev/null 2>&1 || true
  rc-service cfsm-lite-agent restart >/dev/null 2>&1 || rc-service cfsm-lite-agent start >/dev/null 2>&1 || true
  sleep 3
  if rc-service cfsm-lite-agent status >/dev/null 2>&1; then
    echo "安装完成。查看运行日志: tail -f $RC_LOG"
  else
    echo "服务没有正常启动，请查看日志: tail -n 50 $RC_LOG" >&2
    exit 1
  fi
fi
