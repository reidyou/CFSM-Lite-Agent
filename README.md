# CFSM Lite

个人自用的极简服务器监控系统，由三部分组成：

| 部分 | 位置 | 运行在哪里 | 作用 |
|---|---|---|---|
| Worker | `worker/` | Cloudflare Workers 和 D1 数据库 | 接收各服务器上报的数据，向 Monito 提供与 CFSM 兼容的读取接口，提供管理网页，离线或恢复时发 Bark 通知 |
| Agent | `agent/` | 每台被监控的 Linux 服务器 | 采集 CPU、内存、磁盘、流量、网速、Ping 延迟与丢包，每分钟向 Worker 上报一次，不监听任何端口 |
| 管理网页 | Worker 的 `/admin` 路径 | 浏览器或手机 | 添加节点、复制安装命令、系统设置、通知设置、Ping 与丢包配置 |

Monito（iOS）只负责显示，通过 CFSM 面板的方式连接 Worker。

## v1.3 更新内容与升级步骤（实时刷新，和原版一致）

**更新内容**

- **Monito 秒级刷新**：以前 Agent 每分钟才上报一次，所以 Monito 约每分钟更新一次。现在做成和原版同样的思路：Agent 平时保持一条空闲的 WebSocket 连接；你一打开 Monito 或管理页，Worker 的「实时推送中心」（一个 Durable Object）会通知所有 Agent「现在有人在看」，Agent 就每隔 1 到 5 秒（默认 2 秒）推送一次完整的实时指标，推送中心在内存里立刻转发给 Monito；你一离开，Agent 自动退回平时的状态。这些实时数据**不写数据库**，所以不会消耗 D1 额度；数据库仍然只在每个「上报间隔」写一次。
- **和你贴的原版四个参数的对应关系**（都在「节点 → 编辑 → 采集、上报与通知」里）：
  - **上报间隔**（30、60、120、180 秒，默认 60）：没人看的时候，Agent 用 HTTP POST 上报并写入数据库的周期。
  - **采集间隔**（0、1、2、5、10 秒）：Lite 不保存历史曲线，这一项目前不起作用，保留只是和原版一致。
  - **连接模式**（自动 / 仅 HTTP POST，默认自动）：「自动」就是上面的实时推送，HTTP POST 同时在后台兜底；「仅 HTTP POST」则永远只按上报间隔上报，不实时。
  - **看盘上报间隔**（1 到 5 秒，默认 2 秒）：有人在看时 Agent 推送的间隔。
- **推送中心里的几个细节**：Agent 的实时帧会带上完整指标（磁盘、Ping 等沿用最近一次的值），所以不会让 Monito 里某些数字短暂变成 0；管理页的节点卡片标题旁会出现「实时通道」小标记，节点页顶部显示当前有几个页面在看、几台 Agent 已连接；Monito 的 `/api/ws` 也由推送中心提供，不再每 15 秒查一次数据库；如果实时推送出问题，HTTP 上报和读取接口照常工作，只是退回约每分钟刷新。
- **免费额度估算**（15 台节点）：没人看的时候，推送中心休眠，Agent 的保活 ping 由 Cloudflare 自动应答，不唤醒它，几乎不消耗；有人在看时，每秒约 7 条消息，Durable Object 的消息按 20 比 1 折算成请求，一小时约 1350 次；时长一小时约 450 GB-秒，免费版每天 13000 GB-秒，足够连续看二十多个小时。
- 注意：这个实时协议是 Lite 自己的，和原版 cfsm-agent 的 WSS 协议**不通用**；原版 Agent 仍然可以用 HTTP POST 上报，只是没有实时刷新。

**升级步骤**（实时推送需要 Durable Object，它不能在 Cloudflare 网页的 Edit code 里粘贴代码来创建，所以要把部署方式改成「GitHub 连接 Cloudflare 自动部署」，原版 CFSM 也是这样部署的，仍然全部在网页上操作）：

1. **把 Worker 代码放进 GitHub 仓库**：打开你放 Agent 的那个 GitHub 仓库，用 Add file → Upload files 把本项目的 `worker` 文件夹整个拖进去（里面有 `src/index.js`、`schema.sql`、`wrangler.toml` 等），同时用新的 `agent` 文件夹和 `.github` 文件夹覆盖旧的，提交。
2. **填写数据库 ID**：在 GitHub 里打开 `worker/wrangler.toml`，点铅笔图标编辑，把 `database_id` 那一行的中文占位符换成你的 D1 数据库 ID（Cloudflare 控制台 → Storage & Databases → D1 → cfsm-lite，页面上的 Database ID），提交。
3. **让 Cloudflare 从 GitHub 部署**：Cloudflare 控制台 → Workers & Pages → 你现有的 `cfsm-lite` → Settings → Builds，选择连接 Git 仓库（Connect），选你的仓库和分支（一般是 `main`），**Root directory（根目录）填 `worker`**，Build command 留空，Deploy command 填 `npx wrangler deploy`，保存。然后触发一次构建（在 Builds 页点 Retry build，或者在 GitHub 里随便提交一次）。
4. **确认部署成功**：在 Worker 的 Settings → Bindings 里应该能看到 `DB`（D1）和 `HUB`（Durable Object）；你之前设置的变量和密钥（`API_SECRET`、`ADMIN_PASSWORD`、`IS_PUBLIC`、`AGENT_REPO` 等）都应该还在，Triggers 里的 Cron 也应该还在。以后改 Worker 代码，直接在 GitHub 里改 `worker/src/index.js` 并提交就会自动部署，不用再去 Edit code 粘贴。
5. **升级 Agent**：在 GitHub 里发布一个新的 Release（Tag 用 `v0.3.0`），等 `release` 任务完成，再到每台服务器上重新执行一遍该节点的安装命令（实时推送只有新版 Agent 才有）。
6. **验证**：打开管理页，节点页顶部应显示「实时推送：已启用」；等 Agent 升级完成后，会显示「N 台 Agent 已连接」，节点标题旁出现「实时通道」；这时打开 Monito，节点卡片应该每 2 到 3 秒刷新一次。

如果暂时不想改部署方式：继续用粘贴 `index.js` 的老办法也能正常工作，只是没有实时推送，Monito 仍约每分钟刷新一次（管理页会显示「实时推送：未启用」）。

## v1.2 更新内容与升级步骤

**更新内容**

- **节点卡片更紧凑，布局向 Monito 靠拢**：标题后面直接跟分组和地区（国旗加国家代码）；标题下一行仍是操作系统、在线时间、Agent 版本；中间分上下两行共 8 块：上排 CPU（带负载）、内存、磁盘、流量，下排网速、上传/下载量（累计）、延迟、丢包；标签和续费、到期信息放在卡片底部，操作按钮在底部右侧。手机上一行一张卡片，屏幕够宽时自动排成两列或三列。
- **手动排序**：节点页右上角「调整顺序」，进入后每个节点一行，用「置顶」「▲」「▼」调整，每次操作立刻保存，Monito 里的顺序同步变化。
- **默认探测目标**：Monito 只能显示一个目标的延迟和丢包。在「Ping」页的「默认显示目标」里选一个（电信、联通、移动或 BGP），管理页的小卡片和 Monito 都只显示它。因为不知道 Monito 具体读取哪一个字段，对外接口里四组字段（`ping_ct`、`ping_cu`、`ping_cm`、`ping_bd` 及对应的丢包）都会填成默认目标的数值，配置里的四个名称也都用默认目标的名称。管理页本身仍然保留真实的四个目标数据：点卡片里的「延迟」或「丢包」就会展开全部目标。
- **币种**：只保留常用的 8 种：$ 美元、¥ 人民币、€ 欧元、£ 英镑、￥JPY 日元、HK$ 港币、C$ 加拿大元、S$ 新加坡元。以前保存过的其他符号不会丢失，仍然可以正常显示。

**升级步骤（只需要换一个文件）**

1. 把新的 `worker/src/index.js` 粘贴到 Cloudflare 里该 Worker 的 Edit code 里，点 Deploy。
2. 不需要改数据库，不需要新增变量，也不需要升级服务器上的 Agent。

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

- **Worker 和管理网页**：自动化测试全部通过（用本机 SQLite 模拟 D1、用模拟的 Durable Object 和 WebSocket、用模拟浏览器页面，覆盖了登录、节点管理、Ping 与通知设置、到期与流量提醒、实时推送中心、Agent 握手校验等）。**但还没有在真实的 Cloudflare 上运行过**，Durable Object 在真实环境里的行为（连接保持、休眠与唤醒、免费额度）我无法在这里验证。
- **Go Agent**：**我这边没有 Go 编译器，所以没有编译过，也没有运行过**。代码按标准库写法逐行检查，并写了单元测试（包括 WebSocket 帧收发、握手和完整的实时会话），第一次真正编译和测试会发生在 GitHub 上（第四节）。如果 `ci` 变红，请把日志原文发给我。
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

想要 Monito 秒级刷新，需要在这之后再按上面「v1.3 更新内容与升级步骤」把部署方式改成 GitHub 连接（实时推送需要 Durable Object）；不改也能用，只是约每分钟刷新一次。

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

- **节点**：每个节点一张紧凑的卡片（见上面的 v1.2 说明），离线节点显示最后一次数据并变灰。点「调整顺序」可以手动排序。点卡片底部的「编辑」，里面分几组：
  - 续费与到期：续费金额、币种（$ 美元、¥ 人民币、€ 欧元、£ 英镑、￥JPY 日元、HK$ 港币、C$ 加拿大元、S$ 新加坡元）、付费周期（月付到五年付）、到期日、是否自动续费，以及「续费一期」按钮。
  - 流量：每月流量上限（例如 500GB、1TB，不写单位按 GB，按 1024 换算）、流量计算方式、每月流量重置日、统计网卡。
  - 采集与通知：上报间隔（30、60、120、180 秒）、是否隐藏、是否关闭该节点的离线通知，以及可选的专用 Ping 目标、标签和备注。
- **Ping**：设置全局的四个探测目标（默认电信、联通、移动、BGP）和它们的显示名称，以及「默认显示目标」（管理页小卡片和 Monito 显示哪一个）。延迟和丢包来自同一轮探测，没有单独的丢包开关。页面下方显示每个节点四个目标的最新读数。
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
- **实时推送需要 Durable Object**：只有按 v1.3 的步骤用 GitHub 连接部署后才有。没有绑定时，Monito 的 `/api/ws` 退回到「每个连接每 15 秒查一次数据库」的简化实现（刷新约十几秒），Agent 的实时通道会被 Worker 拒绝（返回 503），Agent 每 5 分钟才重试一次，HTTP 上报不受影响。
- 实时数据只在推送中心的内存里转发，不写数据库；如果推送中心被 Cloudflare 回收，重启后会先等 Agent 下一次推送再继续，Monito 最多卡几秒。
- 原版 cfsm-agent 的 WSS 协议和 Lite 的不通用，原版 Agent 只能用 HTTP POST。
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
9. 升级到 v1.2 后：在「Ping」页把默认显示目标改成另一个，保存后约几十秒内，Monito 里的延迟和丢包应变成该目标的数值；管理页节点卡片是 8 块内容、分组和地区紧跟在名称后面；「调整顺序」里置顶一个节点后，Monito 里的顺序也随之变化。
10. 给一个节点设置一个 3 天后到期的日期、流量上限设成略低于当前用量，在「通知」页点保存后等几分钟（流量每 5 分钟检查一次；到期提醒要等到设定的小时之后），确认手机收到到期和流量两条 Bark。
11. 升级到 v1.3 后（按「v1.3 更新内容与升级步骤」部署并升级 Agent）：
    1. 管理页节点页顶部显示「实时推送：已启用」，以及「N 台 Agent 已连接」，节点标题旁有「实时通道」小标记；
    2. 打开 Monito，节点卡片的 CPU 和网速大约每 2 到 3 秒变化一次（在节点编辑里把「看盘上报间隔」改成 1 秒或 5 秒，变化频率应随之改变）；
    3. 关闭 Monito 约 30 秒后，管理页顶部显示「现在有 0 个页面或 App 在看」；
    4. 在节点编辑里把某个节点的「连接模式」改成「仅 HTTP POST」，保存后该节点的「实时通道」标记在几十秒内消失，Monito 里它退回约每分钟刷新；
    5. 如果管理页一直显示「Agent 已连接 0 台」，在服务器上执行 `journalctl -u cfsm-lite-agent -n 50`，把日志发给我。
12. 观察一天 Cloudflare 的 Workers 请求、D1 写入量，以及 Durable Objects 的请求数和时长（预计 15 台每天约 2 万次上报，远低于免费额度；Durable Object 只在有人看时才消耗）。

## 十一、文件清单

- `worker/src/index.js`：Worker 的全部代码（单文件，管理网页、安装脚本和实时推送中心都在里面）。
- `worker/wrangler.toml`：Worker 的部署配置（数据库、实时推送中心、定时任务），用 GitHub 连接 Cloudflare 自动部署时需要，里面要填你的 D1 数据库 ID。
- `worker/schema.sql`：D1 建表语句。
- `worker/test/run.mjs`、`worker/package.json`：自动化测试，只在有 Node.js 的电脑上运行（`node test/run.mjs`），线上部署不需要。
- `agent/`：Go Agent 源码（采集、HTTP 上报、WebSocket 实时推送）、单元测试和 `install.sh`。
- `.github/workflows/`：GitHub 自动检查和发布流程（`ci.yml` 在每次提交时检查并测试，`release.yml` 在发布 Release 时编译两个架构的 Agent）。
