# CFSM Lite

个人自用的极简服务器监控系统，由三部分组成：

| 部分 | 位置 | 运行在哪里 | 作用 |
|---|---|---|---|
| Worker | `worker/` | Cloudflare Workers 和 D1 数据库 | 接收各服务器上报的数据，向 Monito 提供与 CFSM 兼容的读取接口，提供管理网页，离线或恢复时发 Bark 通知 |
| Agent | `agent/` | 每台被监控的 Linux 服务器 | 采集 CPU、内存、磁盘、流量、网速、Ping 延迟与丢包，每分钟向 Worker 上报一次，不监听任何端口 |
| 管理网页 | Worker 的 `/admin` 路径 | 浏览器或手机 | 添加节点、复制安装命令、系统设置、通知设置、Ping 与丢包配置 |

Monito（iOS）只负责显示，通过 CFSM 面板的方式连接 Worker。

## v1.1 更新内容与升级步骤

**本次更新**

1. **Monito 的「连接中断，正在重试」提示**：原因很可能是 Monito 在连 `/api/ws`（实时推送），而 Worker 之前对它直接返回 503。现在 Worker 会真正接受这个 WebSocket 连接，并把新上报的数据推送过去（第九节说明了实现方式和限制）。
2. **磁盘容量改为所有本地磁盘合计**：只统计本机块设备上的 ext4、xfs、btrfs 等文件系统，同一块盘被挂载多次只算一次；NFS、CIFS、sshfs 等网络挂载、tmpfs、overlay、snap 的 loop 镜像都不计入。
3. **管理页的节点卡片信息补全**：CPU 与负载、内存、磁盘、实时网速、当月上下行流量（有上限时显示进度条）、累计流量、四个目标的延迟与丢包、在线时长、系统、地区、Agent 版本、到期日与续费金额。
4. **续费与到期**：每个节点可以设置续费金额、币种、付费周期（月付、季付、半年付、年付、两年付至五年付）、到期日、是否自动续费；编辑里有「续费一期」按钮，按所选周期把到期日顺延。到期前通过 Bark 提醒，默认在到期前 7、3、1 天和到期当天提醒，过期后再提醒一次；提醒天数和每天几点之后发送都可以在「通知」页修改。
5. **流量配额**：每个节点可以设置每月流量上限（例如 500GB、1TB）、流量计算方式（上下行合计、仅上行、仅下行、取较大值）和每月流量重置日（0 表示不重置）。当月用量达到设定百分比（默认 90% 和 95%，可改）时发 Bark 提醒，每个统计周期每个档位只提醒一次。
6. **安装位置**：沿用你修改过的 `/opt/cfsm-lite`；systemd 保护项由 `ProtectHome=yes` 改为 `ProtectHome=read-only`，这样单独挂载在 `/home` 的磁盘也能被统计到。

**升级步骤（不需要改数据库，也不需要新增 Cloudflare 变量）**

1. **Worker**：在 Cloudflare 控制台打开 Worker 的 Edit code，把新的 `worker/src/index.js` 全部粘贴进去替换，点 Deploy。做完这一步，Monito 的提示应该就会消失，管理页的新功能也立即可用。
2. **Agent 程序**：在 GitHub 仓库里用新的 `agent` 文件夹覆盖上传（Add file → Upload files），提交后等 Actions 里的 `ci` 变绿；再发布一个新的 Release（Tag 例如 `v0.2.0`），等 `release` 任务完成。
3. **每台服务器上的 Agent**：重新执行一遍该节点的安装命令（管理页「节点 → 安装命令」里复制）。磁盘合计只有在升级 Agent 之后才生效。脚本会覆盖旧程序并重启服务，节点 ID 和流量累计不受影响。

## 一、当前验证状态（请先读）

- **Worker 和管理网页**：29 项自动化测试全部通过（用本机 SQLite 模拟 D1，用模拟浏览器页面把登录、加节点、改 Ping 与通知设置、改节点、导出、删除完整走了一遍；到期提醒、流量提醒、WebSocket 会话也都有测试）。**但还没有在真实的 Cloudflare 上运行过**，也没有用真实的 Monito 验证 WebSocket。
- **Go Agent**：**我这边没有 Go 编译器，所以没有编译过，也没有运行过**。代码已经按标准库写法逐行检查，并写了单元测试（包括磁盘挂载解析的测试），但第一次真正编译会发生在 GitHub 上（第四节）。如果那里报错，请把报错日志原文发给我。
- **Monito**：没有用真机验证。我没有读过上游源码，协议依据是上游的 API.md 和 Agent 说明文档。

## 二、一次完整部署的顺序

1. 在 Cloudflare 上部署 Worker（第三节）。
2. 在 GitHub 上建仓库并发布 Release，得到 Agent 程序（第四节）。
3. 回到 Cloudflare 设置变量 `AGENT_REPO`，然后在管理网页里添加节点、复制安装命令（第五节）。
4. 在服务器上执行安装命令，在 Monito 里查看（第六节）。

## 三、部署 Worker（全部在 Cloudflare 控制台网页上完成）

菜单名称以写作时的控制台为准，改版后可能略有不同，找意思相近的入口即可。

1. **建数据库**：Storage & Databases 下的 D1，新建数据库，名称填 `cfsm-lite`。
2. **建表**：打开该数据库的 Console 页，把 `worker/schema.sql` 的全部内容粘贴进去执行。如果提示一次只能执行一条语句，就把两段 `CREATE TABLE` 分两次执行。
3. **建 Worker**：Workers & Pages 里新建一个 Worker（Hello World 模板即可），名称填 `cfsm-lite`，先部署一次，再点 Edit code，把 `worker/src/index.js` 的全部内容粘贴进去替换原有代码，点 Deploy。
4. **绑定数据库**：Worker 的 Settings 里添加 D1 database 绑定，变量名必须是 `DB`，选择 `cfsm-lite`。
5. **设置变量和密钥**（Settings 里的 Variables and Secrets）：

| 名称 | 类型 | 说明 |
|---|---|---|
| `API_SECRET` | Secret | 上报密钥，所有节点共用。用 32 位以上只含字母和数字的随机字符串（安装脚本只允许字母、数字和 . _ : + / = ? @ -） |
| `ADMIN_PASSWORD` | Secret | 管理员密码，可选；不设置时等于 `API_SECRET`。建议单独设置并使用强密码，因为登录失败没有次数限制 |
| `ADMIN_USERNAME` | Text | 管理员用户名，可选，默认 `admin` |
| `IS_PUBLIC` | Text | 填 `false`（推荐）：读取监控数据必须先登录。如果 Monito 连接后一直提示未授权，再改为 `true` |
| `AGENT_REPO` | Text | 第四节建好 GitHub 仓库后再填，格式 `用户名/仓库名` |

6. **定时任务**：Settings 的 Triggers 里添加 Cron Trigger，表达式填 `*/1 * * * *`（每分钟一次，用来检测离线）。
7. **验证**：访问 `https://你的Worker域名/` 应看到 `CFSM Lite is running.`；访问 `https://你的Worker域名/admin` 应看到登录页，用户名 `admin`，密码为 `ADMIN_PASSWORD`（没设置则为 `API_SECRET`）。

如果服务器在中国大陆，建议在 Worker 的设置里绑定你自己的域名，不要使用 `workers.dev` 的域名。以后修改代码：在 Edit code 里粘贴新版 `index.js` 后 Deploy，数据库和密钥不受影响。

## 四、用 GitHub 编译 Agent（全部在网页上完成）

1. 在 GitHub 新建一个**公开**仓库（Agent 的下载地址需要公开访问），例如 `cfsm-lite`。
2. 点 Add file → Upload files，把本项目的 `agent` 文件夹整个拖进去，提交。
3. 点 Add file → Create new file，文件名输入 `.github/workflows/ci.yml`（输入斜杠会自动建文件夹），把本项目 `.github/workflows/ci.yml` 的内容粘贴进去，提交。用同样方法再建 `.github/workflows/release.yml`。
4. 打开仓库的 Actions 页，应该能看到名为 `ci` 的任务自动运行。**这是 Go 代码的第一次真实编译和测试**，变绿表示通过。变红就点进去，把报错日志原文发给我。
5. `ci` 通过后，进入仓库的 Releases → Draft a new release，Tag 输入 `v0.1.0` 并选择 Create new tag，点 Publish release。
6. 再回到 Actions 页，等 `release` 任务完成（约 1 到 2 分钟）。完成后，Release 页面里会多出 `cfsm-lite-agent-linux-amd64`、`cfsm-lite-agent-linux-arm64` 和 `SHA256SUMS` 三个文件。
7. 回到 Cloudflare，把 Worker 的变量 `AGENT_REPO` 填成 `用户名/仓库名`，保存并部署。

## 五、添加节点

1. 打开 `https://你的Worker域名/admin` 并登录，在「节点」页输入名称，点「添加节点」。
2. 页面会自动展开该节点的安装命令，点「复制命令」。命令里含有上报密钥，请不要发给别人。

## 六、在服务器上安装 Agent

在要监控的服务器上，用 root 执行刚才复制的命令（非 root 用户在 `sh` 前加 `sudo`）。命令形如：

```
curl -fsSL https://你的Worker域名/install.sh | sh -s -- -i 节点ID -s 上报密钥 -u https://你的Worker域名/update
```

脚本会：下载对应 CPU 架构的程序并校验 SHA-256；把程序放到 `/opt/cfsm-lite/cfsm-lite-agent`，把地址、ID、密钥写入 `/opt/cfsm-lite/agent.env`（文件权限 600，只有 root 能读）；创建 systemd 服务 `cfsm-lite-agent`（以临时的低权限用户运行，不拥有任何额外权限）并启动。

- 查看日志：`journalctl -u cfsm-lite-agent -f`
- 卸载：`curl -fsSL https://你的Worker域名/install.sh | sh -s -- --uninstall`
- 升级：在 GitHub 发布新 Release 后，重新执行同一条安装命令即可（可用 `-v v0.1.1` 指定版本）。

2 分钟内，管理网页里该节点应变为在线。然后在 Monito 里按添加 CFSM 面板的方式，填 Worker 地址、管理员用户名和密码。

## 七、管理网页说明

- **节点**：每个节点一张卡片，显示在线状态、CPU 与负载、内存、磁盘、实时网速、当月和累计流量、四个目标的延迟与丢包、到期日与续费金额；离线节点显示最后一次数据并变灰。编辑里分几组：
  - 续费与到期：续费金额、币种、付费周期、到期日、是否自动续费，以及「续费一期」按钮。
  - 流量：每月流量上限（例如 500GB、1TB，不写单位按 GB，按 1024 换算）、流量计算方式、每月流量重置日、统计网卡。
  - 采集与通知：上报间隔（30、60、120、180 秒）、是否隐藏、是否关闭该节点的离线通知，以及可选的专用 Ping 目标、标签和备注。
- **Ping**：设置全局的四个探测目标（默认电信、联通、移动、BGP）和它们的显示名称。延迟和丢包来自同一轮探测，没有单独的丢包开关。页面下方显示每个节点最新的延迟和丢包读数。
- **通知**：Bark 地址或设备 Key（只写入，不会再显示出来）、离线多久后通知（关闭或 2 到 30 分钟，默认 3 分钟）、通知时间的时区、发送测试通知。离线判断和客户端显示在线或离线用同一个时间。选「关闭全部通知」会同时关闭到期和流量提醒。
  - 到期提醒：到期前几天提醒（默认 `7,3,1,0`，0 表示到期当天，留空表示不提醒）和每天几点之后发送（默认 9 点，按上面的时区）。每个节点的每个到期日在每个档位只提醒一次；过期后再提醒一次；续费后把到期日改到新日期，就会重新开始计算。
  - 流量提醒：当月流量用到百分之几时提醒（默认 `90,95`，留空表示不提醒）。每个统计周期每个档位只提醒一次，新周期开始后重新计算。节点离线时不据旧数据提醒。
- **系统**：站点标题、当前配置信息、导出和导入节点配置（用于备份和迁移，导入后节点 ID 不变，Agent 不用重装）。
- 管理员密码、上报密钥和访问模式不能在网页里改，请到 Cloudflare 控制台修改 Worker 变量。
- 在管理网页里修改的 Ping 目标、探测方式、上报间隔、重置日、统计网卡，Agent 会在下一次上报（约 1 分钟内）自动拉取并应用，不用重启。

## 八、Agent 的行为

- **上报内容**：CPU、内存、Swap、所有本地磁盘合计的容量与已用量、负载、开机时间、累计与当月流量、实时上下行速率、系统信息、进程数、TCP 与 UDP 连接数、IPv4 与 IPv6 是否可用、四个目标的延迟与丢包。
- **Ping 探测**：对每个目标做 3 次 TCP 连接（目标没写端口时用 80），延迟取成功次数的平均值，丢包率为失败次数的比例；全部失败时延迟显示为超时，丢包 100%。目标主动拒绝连接也说明网络通，计为成功。**Lite Agent 不支持 ICMP**，选择 ICMP 时会自动改用 TCP。
- **当月流量**：由 Agent 按「每月流量重置日」累计，状态保存在 `/var/lib/cfsm-lite/state.json`，重启后接着累计。从安装那一刻开始累计，不包含安装之前的流量。
- **磁盘**：读取 `/proc/self/mounts`，只统计文件系统类型为 ext2/3/4、xfs、btrfs、f2fs、jfs、reiserfs、vfat、exfat、ntfs、ntfs3、fuseblk 且设备在 `/dev/` 下的挂载；loop（snap）、ram、zram、nbd 设备不算；同一块设备挂载多次只算一次，所以 bind 挂载和 btrfs 子卷不会重复累加；NFS、CIFS、sshfs、9p、tmpfs、overlay 等一律不计入（也不会去访问它们，避免网络盘卡住）。如果一个都没识别出来（例如部分容器环境），退回到只统计根目录。
- **流量统计网卡**：默认汇总所有网卡，排除 lo、docker、veth、br-、virbr、cni、flannel、kube、cali；可在节点编辑里指定网卡。
- **安全**：只主动向 Worker 发 HTTPS 请求；不监听端口；不执行任何远程命令；不会自动更新；不跟随重定向；日志不输出密钥；只接受白名单里的几项配置，且任何一项不合法就整体忽略。
- **只支持** Linux（amd64 和 arm64）和 systemd，不支持 Windows、macOS、OpenWrt、Alpine 等。

## 九、已知限制

- 延迟和历史图是空的：Lite 不保存历史数据。
- Bark 连续三次发送失败时，这次离线或恢复通知会丢失，不会补发。
- Worker 自己不可用时没有任何告警。
- 登录失败没有次数限制，请使用强密码。
- **WebSocket 推送是「没有 Durable Object」的简化实现**：每个连接每 15 秒查询一次数据库，把新上报的节点推送给客户端；收到客户端的 ping 时也顺带查询一次（两次查询至少间隔 4 秒）。因此推送延迟最多约十几秒，对每分钟上报一次的数据没有影响。免费版对「单次调用里的数据库查询次数」有上限，如果连接存活太久触顶，Worker 会主动断开让客户端自动重连，这时 Monito 可能短暂闪一下提示。这个方案我没有用真实的 Monito 验证过；如果提示仍然存在，请告诉我 Monito 显示的完整文字，下一步可以改用 Durable Object（需要改成用 GitHub 连接 Cloudflare 自动部署）。
- 到期提醒和流量提醒的去重标记保存在 `settings` 表里以 `ntf:` 开头的几行，不影响设置，删除节点时会一并清除。
- 流量上限按 1024 换算（1TB 等于 1024GB）；不同厂商的口径可能不同，必要时把上限写小一点。
- `/api/config` 里的 `version` 是兼容用的固定值，不是真实版本号。
- 编辑节点是「只更新传入的字段」，与上游的整行覆盖不同。
- 节点表里的 `ping_mode` 列不再使用，探测方式只看全局设置。
- 没有流量修正和自动发现建号功能。

## 十、首次真机验证清单（按顺序）

1. `https://你的Worker域名/admin` 能登录；添加节点后能看到安装命令。
2. GitHub Actions 的 `ci` 通过（Go 代码编译成功）；Release 里有三个文件。
3. 安装 Agent 后 2 分钟内，节点变为在线，CPU、内存、网速有数值；`journalctl -u cfsm-lite-agent` 里没有持续报错。
4. 管理网页「Ping」页里延迟和丢包有读数。
5. Monito 能连接并看到服务器列表。若一直提示未授权，把 `IS_PUBLIC` 改成 `true` 并重新部署，再告诉我。
6. 在管理网页改 Ping 目标或上报间隔，约 1 分钟后读数或日志里能看到变化。
7. 关闭某台服务器的 Agent，约 3 分钟后手机收到离线 Bark；重启后收到恢复 Bark。
8. 升级到 v1.1 后：Monito 里的「连接中断，正在重试」提示消失；管理页里每台节点的磁盘容量等于该服务器所有本地磁盘之和（可用 `df -h` 核对，网络挂载不算）。
9. 给一个节点设置一个 3 天后到期的日期、流量上限设成略低于当前用量，在「通知」页点保存后等几分钟（流量每 5 分钟检查一次；到期提醒要等到设定的小时之后），确认手机收到到期和流量两条 Bark。
10. 观察一天 Cloudflare 的 Workers 请求和 D1 写入量（预计 15 台每天约 2 万次，远低于免费额度）。

## 十一、文件清单

- `worker/src/index.js`：Worker 的全部代码（单文件，管理网页和安装脚本都内嵌在里面，直接粘贴到 Cloudflare 即可）。
- `worker/schema.sql`：D1 建表语句。
- `worker/test/run.mjs`：自动化测试，只在有 Node.js 的电脑上运行（`node test/run.mjs`），线上部署不需要。
- `agent/`：Go Agent 源码、单元测试和 `install.sh`。
- `.github/workflows/`：GitHub 自动检查和发布流程。
