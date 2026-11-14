# CFSM Lite Agent

CFSM Lite 的服务器探针（Agent）。装在每台被监控的 Linux 服务器上，采集 CPU、内存、磁盘、流量、网速、Ping 延迟与丢包，上报给 CFSM Lite 的 Worker；有人打开 Monito 或管理网页时自动切换到秒级实时推送。

本仓库（**CFSM-Lite-Agent**，建议设为**公开**）只包含 Agent 的源码、安装脚本和自动编译发布流程。服务端在另一个仓库 **CFSM-Lite**（建议设为**私有**），Agent 需要和它配合使用；添加节点、生成安装命令、查看数据都在服务端的管理网页里进行。

---

## 目录

1. [项目简介](#1-项目简介)
2. [系统要求](#2-系统要求)
3. [获取程序：构建与发布](#3-获取程序构建与发布)
4. [安装与管理](#4-安装与管理)
5. [工作方式](#5-工作方式)
6. [上报的指标](#6-上报的指标)
7. [程序参数与环境变量](#7-程序参数与环境变量)
8. [安全设计](#8-安全设计)
9. [常见问题与排错](#9-常见问题与排错)
10. [验证状态与已知限制](#10-验证状态与已知限制)
11. [项目结构与开发](#11-项目结构与开发)
12. [Alpine 与 Armbian 说明](#12-alpine-与-armbian-说明)

---

## 1. 项目简介

### 1.1 它做什么

- **采集**：CPU 占用与负载、内存与 Swap、所有本地磁盘合计的容量与用量、累计与当月的上下行流量、实时上下行网速、系统信息、进程数、TCP 与 UDP 连接数、IPv4 与 IPv6 是否可用、四个目标的 Ping 延迟与丢包。
- **上报**：每个「上报间隔」（默认 60 秒）用 HTTP POST 向 Worker 上报一次；同时保持一条空闲的 WebSocket 连接，有人在看时每 1 到 5 秒推送一次实时数据，没人看时自动退回。
- **按需配置**：本地只保存 Worker 地址、节点 ID 和上报密钥；上报间隔、Ping 目标、每月流量重置日、统计网卡、连接模式等都由 Worker 在上报时下发，在管理网页里修改后约 1 分钟内自动生效，不用重启或重装。
- **安全简单**：只主动向 Worker 发 HTTPS 请求，不监听端口，不执行任何远程命令，不自动更新，只用 Go 标准库，没有第三方依赖。

### 1.2 与服务端的关系

| 仓库 | 可见性 | 作用 |
|---|---|---|
| `CFSM-Lite` | 私有 | 服务端：Cloudflare Workers、D1 数据库、实时推送、管理网页、Bark 通知 |
| `CFSM-Lite-Agent`（本仓库） | 公开 | Agent 的源码、安装脚本，以及通过 GitHub Release 发布的程序文件 |

**为什么本仓库要公开**：服务器通过 `curl` 从本仓库的 Release 下载程序，服务器上不应该保存任何 GitHub 凭据，所以仓库必须公开。仓库里没有任何密钥；节点 ID 和上报密钥只在你执行安装命令时提供，保存在服务器本机。

下文把本仓库写作 `用户名/CFSM-Lite-Agent`，其中「用户名」换成你自己的 GitHub 用户名。

### 1.3 数据流

```
Agent ──① 每个上报间隔 HTTP POST /update（带完整指标；响应里携带新配置）──▶ Worker ─▶ D1 数据库
  │
  └─② WebSocket（平时只发保活 ping；有人在看时每 1~5 秒推送完整指标）──▶ 实时推送中心 ─▶ Monito / 管理网页
```

---

## 2. 系统要求

- Linux，CPU 架构 x86_64（amd64）或 arm64（包括 64 位的 Armbian 开发板）。
- 使用 systemd（Debian、Ubuntu、Armbian、CentOS、Rocky、Fedora 等常见发行版都是），或 OpenRC（Alpine）。
- 已安装 `curl`（Alpine 上缺少时脚本会自动用 `apk` 补装）；安装时需要 root 权限。
- 服务器能访问你的 Worker 域名（HTTPS，443 端口）；安装时还要能访问 `github.com`（下载程序）。
- **不支持** Windows、macOS、OpenWrt 等系统。
- 服务器在中国大陆时，建议给 Worker 绑定自己的域名，不要用 `workers.dev`（见服务端 README）。

---

## 3. 获取程序：构建与发布

程序由 GitHub 在线编译，不需要在自己的电脑上装 Go，全部在网页上完成。

### 3.1 创建公开仓库并上传文件

1. 在 GitHub 新建仓库，名称 **`CFSM-Lite-Agent`**，选择 **Public（公开）**。
2. 把本项目压缩包解压，在仓库页面点 Add file → Upload files，把解压出来的文件拖进去（不是拖压缩包本身）：`README.md`、`go.mod`、`install.sh`，以及所有 `.go` 文件，提交。
3. 文件夹 `.github` 在 Mac 的访达里默认是隐藏的，不容易拖。改用网页创建：点 Add file → Create new file，文件名输入 `.github/workflows/ci.yml`（输入斜杠会自动建文件夹），把压缩包里同名文件的内容粘贴进去，提交；再用同样的方法建 `.github/workflows/release.yml`。

### 3.2 确认自动编译通过

1. 打开仓库的 Actions 页，应该能看到名为 `ci` 的任务自动运行。它会做静态检查、运行单元测试，并对 amd64 和 arm64 各编译一次（测试带 `-race` 竞态检测）。
2. 变成绿色表示通过。**这也是代码第一次真正编译**，如果变红，点进去看日志，把报错原文发给我，不要继续往下做。

### 3.3 发布 Release

1. 进入仓库的 Releases → Draft a new release。
2. Tag 输入 `v1.0.0`，选择 Create new tag，标题随意，点 Publish release。
3. 回到 Actions 页，等名为 `release` 的任务完成（约 1 到 2 分钟）。它会编译两个架构的程序，生成校验文件，并上传到这个 Release。
4. 打开 Release 页面，应该能看到三个文件：`cfsm-lite-agent-linux-amd64`、`cfsm-lite-agent-linux-arm64`、`SHA256SUMS`。

### 3.4 以后发布新版本

修改代码后提交，等 `ci` 通过，再发布一个新的 Release（Tag 递增，例如 `v1.0.1`）。已经安装的 Agent 不会自动更新，需要在服务器上重新执行安装命令（见 4.5）。

---

## 4. 安装与管理

### 4.1 方式一（推荐）：用服务端管理页生成的命令

在 CFSM-Lite 的管理网页里添加节点后，会生成该节点的安装命令（节点 ID、上报密钥、上报地址都已填好），复制到服务器上用 root 执行即可：

```
curl -fsSL https://你的Worker域名/install.sh | sh -s -- -i 节点ID -s 上报密钥 -u https://你的Worker域名/update
```

非 root 用户在 `sh` 前面加 `sudo`。这条命令里的 `install.sh` 由 Worker 提供，Worker 已经把「从哪个仓库下载程序」填好（来自服务端的变量 `AGENT_REPO`，值为 `用户名/CFSM-Lite-Agent`）。命令里含有上报密钥，请不要发给别人。

### 4.2 方式二：直接用本仓库里的脚本

不经过 Worker，直接从本仓库下载脚本，用环境变量 `CFSM_AGENT_REPO` 指定发布仓库：

```
curl -fsSL https://raw.githubusercontent.com/用户名/CFSM-Lite-Agent/main/install.sh | CFSM_AGENT_REPO=用户名/CFSM-Lite-Agent sh -s -- -i 节点ID -s 上报密钥 -u https://你的Worker域名/update
```

### 4.3 脚本的参数

| 参数 | 必填 | 说明 |
|---|---|---|
| `-i 节点ID` | 是（安装时） | 在管理网页里添加节点后得到的 ID |
| `-s 上报密钥` | 是（安装时） | 服务端变量 `API_SECRET` 的值 |
| `-u 上报地址` | 是（安装时） | `https://你的Worker域名/update`，必须以 `https://` 开头 |
| `-v 版本` | 否 | 指定安装哪个版本，例如 `v1.0.0`；不指定就安装最新的 Release |
| `--uninstall` | 否 | 卸载 |

环境变量 `CFSM_AGENT_REPO`：仅在方式二（脚本没有被 Worker 填好仓库）时需要，格式 `用户名/CFSM-Lite-Agent`。

节点 ID、密钥和上报地址里只允许字母、数字和 `. _ : + / = ? @ -` 这些字符。

### 4.4 脚本做了什么

1. 检查是 root、服务管理器是 systemd 或 OpenRC（Alpine）、有 curl（Alpine 上缺少 `curl`、`ca-certificates` 时自动补装）、参数齐全、上报地址以 `https://` 开头。
2. 根据 CPU 架构下载对应的程序，同时下载 `SHA256SUMS` 校验文件并核对；校验失败、下载不到校验文件、或系统里没有 `sha256sum`，都会中止安装（不会装一个没校验过的程序）。确认风险后确实需要跳过时，在命令的 `sh -s --` 后面加参数 `--skip-verify`。下载只走 HTTPS（重定向也一样）、至少 TLS 1.2，网络抖动时自动重试 3 次，连接超过 10 秒、程序下载超过 300 秒（校验文件 60 秒）会放弃，不会一直卡住。
3. 把程序放到 `/opt/cfsm-lite/cfsm-lite-agent`。
4. 把上报地址、节点 ID、密钥写入 `/opt/cfsm-lite/agent.env`，文件权限 600，只有 root 能读。
5. 创建服务 `cfsm-lite-agent` 并启动：systemd 系统是 `/etc/systemd/system/cfsm-lite-agent.service`；Alpine 是 OpenRC 脚本 `/etc/init.d/cfsm-lite-agent`（用 supervise-daemon 守护，异常退出 10 秒后重启，日志写到 `/var/log/cfsm-lite-agent.log`，超过 5MB 时在下次启动时清空；Alpine 上无法使用下面 systemd 的沙箱保护项）。两种系统的程序、`agent.env`、`state.json` 路径完全相同，创建的服务用户也都是 `cfsm-lite`。下面的描述以 systemd 为准：服务以专门创建的低权限系统用户 `cfsm-lite` 运行（只能写 `/opt/cfsm-lite` 这一个目录），并开启这些保护：`ReadWritePaths` 只开放该目录、`NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome=read-only`、`PrivateTmp`、不授予任何额外权限（`CapabilityBoundingSet` 为空）；以及一组扩展加固：`PrivateDevices`、`ProtectKernelTunables`、`ProtectKernelModules`、`ProtectKernelLogs`、`ProtectControlGroups`、`ProtectClock`、`ProtectHostname`、`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`（只允许 IPv4、IPv6 和本地套接字）、`RestrictNamespaces`、`RestrictRealtime`、`RestrictSUIDSGID`、`LockPersonality`、`SystemCallArchitectures=native`、`UMask=0077`，并限制内存 `MemoryMax=128M` 和线程数 `TasksMax=128`（`systemd-analyze security` 的评分从 5.9 降到 3.1）。扩展加固刻意没有包含 `ProtectProc=invisible`，它会让「进程数」只统计到 Agent 自己。如果个别系统不接受这些扩展项导致服务启动失败，脚本会打印警告，自动退回只含基础保护的服务文件再试一次`ProtectHome` 用只读而不是完全隐藏，是为了让单独挂载在 `/home` 的磁盘也能被统计到。
6. 当月流量的累计状态保存在 `/opt/cfsm-lite/state.json`（和程序在同一目录；服务以专门的系统用户 `cfsm-lite` 运行，只有这个目录归它所有，程序和 `agent.env` 仍归 root）。同一个文件里还保存着最近一次从 Worker 收到的配置（重置日、重置时刻、时区、统计网卡等），状态文件先写临时文件并 `fsync`，再改名替换，断电也不会留下空文件或半截文件，并保留一份最多落后约 10 分钟的备份 `state.json.bak`；主文件万一损坏，Agent 会在日志里说明，自动用备份恢复（没有备份才从 0 开始，损坏的文件保留为 `state.json.corrupt`）。所以重启服务或重新执行安装命令升级后，Agent 会先按这份配置接着累计，不会因为尚未连上 Worker 而用默认值把当月流量清零。

### 4.5 日常管理

| 想做的事 | 命令 |
|---|---|
| 查看运行日志 | `journalctl -u cfsm-lite-agent -f`（Alpine：`tail -f /var/log/cfsm-lite-agent.log`） |
| 查看服务状态 | `systemctl status cfsm-lite-agent`（Alpine：`rc-service cfsm-lite-agent status`） |
| 重启 | `systemctl restart cfsm-lite-agent`（Alpine：`rc-service cfsm-lite-agent restart`） |
| 升级 | 发布新 Release 后，重新执行同一条安装命令（节点 ID 和流量累计不受影响） |
| 安装指定版本 | 在安装命令的 `sh -s --` 后面加 `-v v1.0.1` |
| 卸载 | 把安装命令里 `sh -s --` 后面的内容换成 `--uninstall` |
| 查看版本 | `/opt/cfsm-lite/cfsm-lite-agent -version` |

卸载会停止并删除服务、程序、`/opt/cfsm-lite` 目录和状态文件。

---

## 5. 工作方式

### 5.1 配置从哪里来

Agent 本地只有三项必需配置：上报地址、节点 ID、上报密钥。其余运行参数都由 Worker 在上报时下发，Agent 只接受下面这些键，其余一律忽略；配置串超过 1024 字节、或其中任何一项不合法，就整体忽略、保持当前配置：

| 配置键 | 取值 | 含义 |
|---|---|---|
| `report_interval` | 30、60、120、180（秒） | 没人看时 HTTP POST 上报的周期，默认 60 |
| `reset_day` | 0 到 31 | 每月流量重置日，0 表示不重置，默认 1 |
| `reset_hour`、`reset_minute` | 0 到 23、0 到 59 | 重置日当天的重置时刻，默认 0 点 0 分 |
| `timezone` | IANA 时区名，如 `Asia/Shanghai` | 重置时刻使用的时区，由服务端「系统」页统一设置，默认上海；程序内嵌了时区数据库，不依赖服务器自带的 tzdata |
| `cal_id`、`cal_rx`、`cal_tx`、`cal_period` | 数字、字节数、周期标识 | 手动校准：把本期下行、上行累计直接改成填写的数值，同一个 `cal_id` 只应用一次，校准所属周期已过则忽略 |
| `interface` | 网卡名，多个用逗号分隔，留空表示自动汇总 | 统计流量的网卡 |
| `custom_ct`、`custom_cu`、`custom_cm`、`custom_bd` | `主机` 或 `主机:端口` | 四个 Ping 目标 |
| `connection_mode` | `auto` 或 `http` | `auto`：保持实时连接；`http`：只用 HTTP POST |
| `wss_report_interval` | 1 到 5（秒） | 实时推送的间隔（最终以服务端通知的为准），默认 2 |

首次连接时，Agent 使用内置的默认值，随后第一次上报就会拿到服务端的配置。

### 5.2 HTTP 上报

- 每个上报间隔向上报地址发一次 `POST`，请求体是 JSON：`id`、`secret`、`metrics`（完整指标）。
- 响应里的 `Date` 头会用于时间校准（见 5.5）。
- 请求头 `X-Agent-Version: Lite-agent-<版本号>`（管理页节点卡片上显示的 Agent 版本就是它）；`X-Agent-Config-Schema: 7` 和 `X-Agent-Config-Md5`（Agent 当前使用的配置的 MD5，首次为 `none`）。
- Worker 的响应：`204` 表示配置没有变化；`200` 且带配置串，表示有新配置，Agent 校验通过后应用，下一次上报带上新的 MD5。
- 不跟随重定向，避免上报内容和密钥被带到别的地址。
- 每一轮采集做 1 秒的 CPU 与网速采样，大约耗时 1 秒。Ping 由后台独立持续采样，上报时只取最新的统计值，不阻塞 HTTP 上报，也不阻塞实时推送；刚启动时第一次上报最多多等 15 秒，让每个目标至少有一个样本。

### 5.3 实时推送

1. **连接**：连接模式为「自动」时，Agent 向上报地址发起 WebSocket 升级请求（`GET /update`，请求头 `Authorization: Bearer <上报密钥>`、`X-Agent-Id: <节点ID>`、`X-Agent-Version`）。连不上不影响 HTTP 上报。
2. **保活**：空闲时每 25 秒发一条文本消息 `ping`，服务端自动回 `pong`；超过 60 秒没收到任何数据就判定连接已死并重连。有人在看（正在推送）时缩短为每 10 秒一次、超过 25 秒没有数据就重连，连接卡死时看盘页面最多停顿几十秒，而不是一分多钟。
3. **服务端通知 Agent**（文本消息，JSON）：
   - `{"type":"ready","active":false,"interval":2}`：连接建立时发送，`active` 表示此刻是否已经有人在看。
   - `{"type":"activate","interval":2}`：有人开始看，Agent 开始实时推送，`interval` 是推送间隔（1 到 5 秒）。已经在推送时再收到同样间隔的 `activate`（服务端发现很久没有收到实时帧时会补发）会被忽略，保持原来的节奏。
   - `{"type":"deactivate"}`：没人看了，Agent 停止推送。
4. **Agent 推送**：`{"type":"metrics","metrics":{…完整指标…}}`。每一帧都带完整指标，变化慢的字段（磁盘、负载、进程与连接数、IP 可用性、系统信息）每 10 秒才重新读取一次，其余沿用；Ping 结果是后台持续采样的最新统计值，每一帧都是新的。
5. **实时数据不写数据库**；数据库仍然每个上报间隔由 HTTP POST 写一次。
6. 管理页里把节点的连接模式改成「仅 HTTP POST」后，Agent 会在几秒内关闭实时连接。

### 5.4 出错时的行为

| 情况 | Agent 的做法 |
|---|---|
| HTTP 上报网络失败 | 记录日志，下个周期继续 |
| HTTP 上报被拒绝（HTTP 400、401、403、404） | 说明节点 ID 或密钥不对，或节点已被删除；至少等 120 秒再试 |
| HTTP 上报返回其他状态码 | 记录日志，下个周期继续 |
| 实时连接握手返回 503 或 403 | 服务端没有启用实时推送，或这个节点被设为仅 HTTP；5 分钟后再试 |
| 实时连接握手返回 400、401、404，或连接被关闭时带 1008、4404 | ID 或密钥有问题，或节点被删；120 秒后再试 |
| 实时连接其他原因断开 | 5 秒后重连，之后每次失败等待时间翻倍，最长 5 分钟；连接稳定超过 1 分钟后恢复为 5 秒。每次等待都会加上 ±20% 的随机量，避免服务端重新部署后所有节点在同一时刻重连 |

实时连接的任何问题都不影响 HTTP 上报；实时连接相关的日志每分钟最多输出一次。

### 5.5 采集细节

- **CPU**：读取 `/proc/stat`，用两次读数的差值计算占用百分比（保留 2 位小数）。HTTP 上报用 1 秒窗口；实时推送用「上一帧到这一帧」的窗口。
- **内存与 Swap**：读取 `/proc/meminfo`，已用 = 总量 − 可用（`MemAvailable`；老内核没有这一项时用空闲、缓冲、缓存估算）。单位 MB。
- **磁盘**：读取 `/proc/self/mounts`，**统计本地磁盘和 ZFS 存储池并合计**（口径参照 Komari Agent）：
  - 统计：文件系统类型为 ext2/3/4、xfs、btrfs、f2fs、jfs、reiserfs、vfat、exfat、ntfs、ntfs3、fuseblk，且设备在 `/dev/` 下的挂载；
  - 不统计：loop（snap 镜像）、ram、zram、nbd 设备；NFS、CIFS、sshfs、9p、tmpfs、overlay 等网络挂载和虚拟文件系统（也不会去访问它们，避免网络盘卡住）；
  - 同一块设备挂载多次只算一次，所以 bind 挂载和 btrfs 子卷不会重复累加；
  - **ZFS**：文件系统类型为 zfs 的数据集也统计；同一个存储池下的多个数据集只算一份，取其中容量最大的那个（和 Komari 一致）。这是近似值：多数据集的存储池、以及存储池里没有挂载的 zvol（例如 Proxmox 的虚拟机磁盘），都不会完整反映在这个数字里，要精确值请以 `zpool list` 为准；
  - **根目录 `/` 始终统计**（和 Komari 一致）：LXC 等容器的根文件系统可能是 loop 设备、ZFS 子卷或 overlay，不在上面的规则里，不算的话磁盘会显示成 0；旧内核可能先列出一行 `rootfs`，只取最后一个 `/`；
  - 如果一个都没识别出来，退回到只统计根目录；
  - **卡死保护**：每个挂载点的用量读取都有 3 秒超时。个别挂载卡死（例如 ntfs-3g 进程挂掉、磁盘故障）时，这个挂载点暂时沿用上一次的数值并在日志里提示（每分钟最多一条），不会再拖住上报和实时推送，也不影响其他磁盘；它恢复后自动回到正常读数。
- **流量**：读取 `/proc/net/dev`。默认汇总所有网卡，排除 `lo`、`br`、`cni`、`docker`、`podman`、`flannel`、`veth`、`virbr`、`vmbr`、`tap`、`fwbr`、`fwpr`（与 Komari Agent 一致）以及 `kube`、`cali`、`vnet`、`tun`、`wg`、`tailscale`、`zt` 开头的虚拟网卡（`vnet` 是 libvirt/KVM 给每台虚拟机创建的网卡；`tun`、`wg`、`tailscale`、`zt` 是 OpenVPN、WireGuard、Tailscale、ZeroTier 的 VPN 隧道网卡）——这些网卡上的流量会从物理网卡再走一遍，算进去就是重复计数；可由「统计网卡」指定，支持 `*`、`?` 通配符（例如 `eth*`），明确写出的网卡不受上述排除限制。**如果这台服务器真正的出口就是隧道网卡**（例如全部流量都走 WireGuard），请在服务端节点编辑的「统计网卡」里明确写出它的名字（如 `wg0`），否则自动汇总会漏算这部分流量。
  - 累计上下行（`net_rx`、`net_tx`）是网卡计数器的原始值；
  - 实时网速由两次读数的差值除以间隔得到，计数器回绕或网卡重置时按 0 处理，不会出现负数；
  - **当月流量**由 Agent 按「每月流量重置日」累计，保存在 `/opt/cfsm-lite/state.json`，重启或重新执行安装命令升级后接着累计（累计状态每 30 秒和退出时落盘）；Agent 停止运行期间产生的流量，会在下次启动时补上（计数器没有被重置的话）。**累计算法与 Komari Agent 对齐**：按网卡分别算两次读数的差值再相加；某块网卡的计数器变小（重启、网卡重建、读数异常）时这一段按 0 处理并重建基线，新出现的网卡首次读数也只建基线，都不会把整个计数器当作新增流量。**从安装那一刻开始累计，不包含安装之前的流量**。重置日大于当月天数时取当月最后一天；统计周期按服务端设置的时区（默认 `Asia/Shanghai`）和重置时刻（默认 0 点 0 分）划分，与服务器本机时区无关；更换统计网卡会重新建立基线；修改重置日、重置时刻后当前周期的累计会从 0 重新开始，需要的话可以在服务端用「手动校准」补回。
- **Ping 与丢包**：采集和计算方式参照 komari-agent，只支持 TCP。后台对四个目标各自每 10 秒采样一次，目标没写端口时用 80。每个样本只测一次 TCP 握手（超时 3 秒），域名先解析（与 komari 一致，取系统解析器返回的第一个地址：本机有可用 IPv6 时通常是 IPv6，否则是 IPv4；解析超时 10 秒），耗时只计算握手；连接失败（包括被拒绝）、解析失败都记为丢包。握手耗时超过 1000ms 时重测，最多 3 次：重测失败记为丢包；重测不超过 1000ms 但比首次少 800ms 以上，说明首次多半是 SYN 丢失后等内核约 1 秒重传，这个样本记为丢包而不是高延迟；3 次重测都超过 1000ms 也记为丢包。丢包率 = 最近 1 小时内的失败样本数 ÷ 总样本数（约 360 个样本）；延迟 = 最近 10 分钟内成功样本的平均值（毫秒，最小 0.1），两者的统计口径与 komari 面板一致。需要排查数据和 komari 对不上时，给 Agent 加 `-debug` 参数，日志里会逐个样本记录解析到的 IP、耗时和失败原因。结果的约定：数字表示有效值；`null` 表示最近 10 分钟内全部失败；`false` 表示这个目标没有配置。目标改了会丢掉旧样本重新统计；刚启动或刚改目标的前几分钟，样本少，丢包率比较粗。
- **时间校准**（`clock.go`）：Agent 用 Worker 响应里的 HTTP `Date` 头估算「Worker 时钟 − 本机时钟」的差值，**只**用来修正两样东西：上报的 `boot_time`（它来自 `/proc/stat` 的 `btime`，是按本机时钟算的，本机时钟不准时面板上的「在线时间」会跟着错）和当月流量统计周期的划分（重置日、重置时刻按「现在几点」判断，本机时钟偏几个小时就会在错误的时间清零）。**不会修改系统时间**，也不影响 CPU、网速等按相对时间计算的指标。
  - **样本来源**：启动时先向 Worker 发一次 `OPTIONS /`（Worker 直接返回 204，不读数据库；不带密钥，只带 `X-Agent-Version`），这样第一次上报之前就已完成校准；之后每次 HTTP 上报的响应、每次实时连接握手的响应都会继续提供样本。失败（网络不通、响应没有 `Date`）不影响其他流程。
  - **估算方法**：`Date` 只精确到秒，取「Date + 0.5 秒」作为 Worker 时刻，本机一侧取请求往返的中点；往返超过 5 秒的样本丢弃。只保留往返最短的样本（更可信），但样本超过 1 小时、或新样本与旧样本相差超过 5 秒（本机时钟被调整过）时会更新。
  - **何时生效**：差值达到 5 秒才开始校准（估算误差在 1 到 2 秒内，小于 5 秒视为准确）；已经在校准时，差值降到 2 秒以下才取消，避免在阈值附近来回切换。生效、取消或变化超过 5 秒时输出一条日志，提示本机时钟偏快或偏慢了多少，并建议检查 NTP 同步。
  - **读数先后**不受影响：累计流量时判断「这组读数是不是比上次更早」仍按本机时间，只有划分统计周期时才加上校准量。
- **IP 可用性**：用 UDP「连接」检查本机是否有到公网的 IPv4、IPv6 路由（不会真的发送数据包），结果为 `1` 或 `0`。
- **连接数**：TCP 只统计 ESTABLISHED 状态的连接；UDP 统计套接字数量。逐行流式扫描 `/proc/net/{tcp,tcp6,udp,udp6}`，连接数很多的机器上也几乎不产生内存垃圾（5 万条连接实测约 5 毫秒）。

---

## 6. 上报的指标

| 字段 | 含义与单位 |
|---|---|
| `cpu` | CPU 占用，百分比 |
| `ram_total`、`ram_used` | 内存总量与已用，MB |
| `swap_total`、`swap_used` | Swap 总量与已用，MB |
| `disk_total`、`disk_used` | 所有本地磁盘的总量与已用，MB |
| `load_avg` | 1、5、15 分钟负载，空格分隔的三个数 |
| `boot_time` | 开机时间，Unix 毫秒（本机时钟偏差达到 5 秒以上时已按 Worker 时间校准，见 5.5） |
| `net_rx`、`net_tx` | 网卡累计下行与上行，字节 |
| `net_rx_monthly`、`net_tx_monthly` | 当月累计下行与上行，字节 |
| `net_in_speed`、`net_out_speed` | 实时下行与上行速率，字节每秒 |
| `os`、`arch`、`kernel_version`、`cpu_info`、`cpu_cores` | 系统、架构（x86_64 或 aarch64）、内核版本、CPU 型号、逻辑核心数。系统名取自 `/etc/os-release` 的 `PRETTY_NAME`；Debian 例外，会读 `/etc/debian_version` 补上小版本号，显示为 `Debian 13.1`（管理页再拼上架构：`Debian 13.1 (x86_64)`） |
| `processes`、`tcp_conn`、`udp_conn` | 进程数、TCP 连接数、UDP 套接字数 |
| `ip_v4`、`ip_v6` | 有无到公网的 IPv4、IPv6 路由，`1` 或 `0` |
| `ping_ct`、`ping_cu`、`ping_cm`、`ping_bd` | 四个目标的延迟，毫秒；`null` 表示全部超时；`false` 表示未配置 |
| `loss_ct`、`loss_cu`、`loss_cm`、`loss_bd` | 四个目标的丢包率，百分比 |

---

## 7. 程序参数与环境变量

一般不需要手动运行程序，systemd 服务会从 `/opt/cfsm-lite/agent.env` 读取环境变量。下面是程序本身支持的参数（每个参数也可以用对应的环境变量提供）：

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `-url` | `CFSM_URL` | 上报地址，例如 `https://example.com/update`；必须是 `https://`，调试时可加 `-allow-http` |
| `-id` | `CFSM_ID` | 节点 ID |
| `-secret` | `CFSM_SECRET` | 上报密钥（建议用环境变量，避免出现在进程列表里） |
| `-state` | `CFSM_STATE` | 状态文件路径，默认 `/opt/cfsm-lite/state.json` |
| `-allow-http` | — | 允许使用 `http://` 上报（密钥会明文传输，不推荐，仅调试用） |
| `-debug` | — | 输出调试日志 |
| `-version` | — | 打印版本号并退出 |

---

## 8. 安全设计

- 只主动向你配置的 Worker 地址发 HTTPS 请求，不监听任何端口，不执行任何来自服务端的命令，没有自动更新。
- 上报地址必须是 `https://`；不跟随重定向，避免密钥被带到别的地址。
- 密钥只保存在 root 才能读的 `/opt/cfsm-lite/agent.env`；日志不输出密钥或完整请求头。
- 只接受白名单里的几项配置，任何一项不合法就整体忽略；配置串有 1024 字节上限。
- systemd 服务以专门的低权限用户运行，不授予额外权限，系统目录只读，家目录只读，并启用扩展加固（见 4.4）。
- 下载程序只走 HTTPS 并带超时和重试（见 4.4）；CI 始终使用当前受支持的最新 Go 版本编译，标准库的安全修复才会进入发布的程序。
- 安装脚本下载程序后用 `SHA256SUMS` 校验。
- 只使用 Go 标准库，没有第三方依赖，也就没有依赖带来的供应链风险。

---

## 9. 常见问题与排错

先看日志：`journalctl -u cfsm-lite-agent -n 50`（Alpine：`tail -n 50 /var/log/cfsm-lite-agent.log`）。

| 日志里的内容 | 含义和处理 |
|---|---|
| `上报被服务端拒绝（HTTP 401 …）` | 上报密钥不对：确认安装命令里的密钥和服务端变量 `API_SECRET` 一致；改过密钥后要重新执行安装命令 |
| `上报被服务端拒绝（HTTP 404 …）` | 节点 ID 不存在或节点已在管理网页里被删除：重新添加节点，用新的安装命令 |
| `上报失败（连续第 N 次）…` 后面是网络错误 | 服务器连不上 Worker 的域名：检查 DNS、防火墙；大陆服务器请换成自己的域名 |
| `实时推送连接断开：WebSocket 握手失败：HTTP 503` | 服务端没有启用实时推送（Worker 没绑定 Durable Object），这不影响 HTTP 上报；按服务端 README 用 GitHub 连接部署 |
| `实时推送连接断开：WebSocket 握手失败：HTTP 403` | 这个节点的连接模式被设成了「仅 HTTP POST」，改回「自动」即可 |
| `忽略无效的配置: …` | 服务端下发的配置里有不合法的值，Agent 保持原配置；检查管理页里该节点的设置 |
| `状态文件 … 损坏（…），尝试从备份恢复` | 状态文件被断电等原因损坏。日志下一行会说明是从备份恢复（最近一小段流量累计可能丢失），还是没有备份、从 0 重新累计；损坏的文件保留为 `state.json.corrupt`，可以删除 |
| `读取磁盘 /xxx 的用量超过 3s 没有返回` | 这个挂载点卡死了（FUSE 进程挂掉、磁盘故障等），Agent 暂时沿用它上一次的数值，其他指标和上报不受影响；请检查该磁盘或挂载 |

其他常见问题：

- **安装时提示下载失败**：确认本仓库是公开的，并且已经发布了 Release，里面有程序文件和 `SHA256SUMS`；服务器要能访问 `github.com`。
- **安装时提示「没有指定发布仓库」**：说明没有用服务端生成的命令，请设置环境变量 `CFSM_AGENT_REPO`（见 4.2）。
- **节点一直不上线**：先看日志；再确认服务器时间正常、系统能访问 Worker 域名。
- **磁盘容量和 `df -h` 对不上**：Agent 只合计本地磁盘和 ZFS 存储池（见 5.5）；网络挂载不计入；ZFS 的数字是近似值。
- **当月流量比服务商面板小**：Agent 从安装那一刻才开始累计；统计网卡不对时也会偏差，可在节点编辑里指定网卡，也可以用「手动校准当期流量」直接填入服务商面板上的上传和下载流量。
- **想暂停某台服务器的实时推送**：在管理页把该节点的连接模式改成「仅 HTTP POST」。

---

## 10. 验证状态与已知限制

### 10.1 验证状态

- **编译与测试**：代码已用 Go 1.24 编译，`go vet` 和单元测试（含 `-race` 竞态检测）通过，其中包括状态文件并发保存与损坏恢复、磁盘卡死保护、连接数流式统计与旧实现的逐行对照、重连抖动；安装脚本生成的 systemd 服务文件已用 `systemd-analyze verify` 校验；单元测试覆盖配置解析、`/proc` 文件解析、磁盘挂载筛选、流量累计、Ping、WebSocket 帧收发与握手、完整的实时会话等。上传到 GitHub 后请仍然先确认 `ci` 是绿色的（见 3.2）。**还没有在真实服务器上长期运行过。** 时间校准（`clock.go` 与 `clock_test.go`）这一部分的代码也已用 Go 1.24 编译，单元测试（含 `-race`）通过；仍然请先看 GitHub 上 `ci` 是否变绿（第 3.2 节），变红就把报错原文发出来。扩展加固项和卡死挂载的行为**还没有在真实的 systemd 和故障磁盘上验证过**。
- 没有在真实的 Cloudflare 服务端和 Monito 上验证过；请按服务端 README 的「首次验证清单」检查。

### 10.2 已知限制

- 只支持 Linux（amd64 和 arm64）以及 systemd 或 OpenRC；32 位 ARM（armv7、armhf）不支持，安装脚本会直接提示不支持的架构。
- 不支持 ICMP 探测，只有 TCP。
- 不保存历史数据，也没有历史曲线。
- **不兼容原版 CF-Server-Monitor 的 cfsm-agent**：上报格式、配置下发和实时推送协议都是本项目自己的，原版 Agent 不能连接本项目的服务端。
- 磁盘只统计本地磁盘和 ZFS 存储池（根目录始终统计）；网络挂载一律不计入；ZFS 为近似值。
- 当月流量从安装时开始累计；与服务商面板有出入时，用服务端节点编辑里的「手动校准当期流量」修正。校准按「Agent 下次上报时」生效，期间产生的少量流量会算进去。
- 服务端的「采集间隔」设置目前不起作用（Lite 不保存历史样本）。

---

## 11. 项目结构与开发

```
CFSM-Lite-Agent/
├── README.md             本文件
├── go.mod                Go 模块（只用标准库）
├── install.sh            安装脚本（也被服务端内嵌了一份拷贝）
├── main.go               入口：参数解析、HTTP 上报主循环
├── report.go             HTTP 上报
├── config.go             配置解析（Worker 下发的配置串）、MD5
├── collect.go            指标采集：快照、完整指标构造、实时采样
├── parse.go              /proc 文件解析、磁盘挂载筛选与合计
├── sys_linux.go          磁盘用量（statfs 系统调用）
├── diskguard.go          磁盘用量读取的超时保护（挂载卡死时沿用上次数值）
├── ping.go               TCP 延迟与丢包：后台采样与窗口统计
├── state.go              当月流量累计与统计周期
├── clock.go              时间校准：用 HTTP Date 头估算本机时钟偏差
├── clock_test.go         时间校准的单元测试
├── realtime.go           实时推送：连接管理、保活、按需推送
├── wsclient.go           最小的 WebSocket 客户端（只用标准库）
├── agent_test.go         解析、配置、状态、Ping、上报等的单元测试
├── realtime_test.go      WebSocket 与实时会话的单元测试
├── hardening_test.go     状态文件恢复、磁盘卡死保护、连接数统计、重连抖动的单元测试
└── .github/workflows/
    ├── ci.yml            每次提交：静态检查、单元测试（含竞态检测）、两个架构的编译检查
    └── release.yml       发布 Release 时：编译两个架构、生成校验文件并上传
```

- 修改代码后提交，等 `ci` 通过；需要发新版本时再发布一个 Release（见 3.4）。
- **`install.sh` 在服务端 CFSM-Lite 仓库的 `src/index.js` 里还有一份拷贝**（常量 `INSTALL_SH`），修改本仓库的 `install.sh` 后，要把同样的内容同步过去，同步后在 `agent` 文件夹里用下面的命令核对两份是否一致（`你的Worker域名` 换成实际域名；它把服务端返回内容里被替换过的 `REPO=` 一行还原后，和本地文件逐行比较）：

  ```sh
  curl -fsSL https://你的Worker域名/install.sh | sed 's#^REPO=.*#REPO="__REPO__"#' | diff - install.sh && echo 一致
  ```

  输出「一致」就说明两份相同；有差异时会列出不同的行。脚本里 `REPO="__REPO__"` 这一行是占位符，保持不变：服务端返回脚本时会把它替换成 `AGENT_REPO` 的值，直接使用本仓库脚本时则读取环境变量 `CFSM_AGENT_REPO`。
- 本地开发（可选，需要 Go 1.21 或更新版本）：`go vet ./...`、`go test ./...`（加 `-race` 做竞态检测需要本机有 C 编译器）。
- 程序的版本号在编译时由 `release.yml` 注入（等于 Release 的 Tag）；本地直接编译得到的版本号是 `dev`。

## 12. Alpine 与 Armbian 说明

- **静态编译**：所有 Release 文件都用 `CGO_ENABLED=0` 编译，不依赖 glibc，Alpine（musl）直接能运行；时区数据库也编进了程序（`time/tzdata`），Alpine 没装 `tzdata` 也不影响。新增依赖时不要引入需要 CGO 的库，否则 Alpine 上会出现 `not found`。安装脚本会在启动服务前试运行一次下载的程序，无法执行时（退出码 126 / 127）直接中止并提示。
- **Armbian**：只支持 64 位（`uname -m` 为 `aarch64`，用 arm64 版本）；32 位系统（`armv7l`、`armv8l`）不支持。ARM 的 `/proc/cpuinfo` 没有 `model name`，CPU 型号依次取 `Hardware`、`Processor`、`Model`，都没有时读设备树 `/proc/device-tree/model`（例如 `Orange Pi 5`）。
- **本机检查**：`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null .`；在 Alpine 容器里跑：`docker run --rm -v "$PWD:/d" alpine:3 /d/cfsm-lite-agent-linux-amd64`，出现 `not found` 说明不是静态编译，程序因为缺少配置而退出则说明二进制没问题。
