# nasctl —— 轻量 NAS 控制台

一个程序，一条原则：**机器不能宕，原文件不能坏，这比功能多少更重要。**

- **`nasctl`** —— NAS 控制台：文件浏览、预览、相册、自动追番、媒体库、
  本地文件备份和任务记录。原生追番提供 RSS 筛选、qBittorrent 下载、归档和可选 TMDB 元数据。
- **`nasctl album`** —— 同一个程序里的相册处理命令。它会扫描目录、生成
  缩略图，但**永远**是有上限的任务：有明确的数量上限、运行时长上限，前面还有
  一道资源闸门可以随时叫停。

日常只需启动 `nasctl serve`：它直接读取本地相册索引。扫描与缩略图由控制台按需
以 `nasctl album ...` 重新启动自身，在独立进程和 cgroup 中执行，不需要另装程序。
Web 查询层的导入图仍不包含扫描器、解码器或子进程实现，只有经权限与资源检查的
显式任务入口能启动有数量、时间上限的后台处理进程。

> 从 0.2.x 或更早版本升级请先读 [`docs/UPGRADE.md`](docs/UPGRADE.md)。原来的 `albumctl` 已并入 `nasctl album`，`albumctl serve` 独立网页和
> `hub.album.api_base` 远程模式已移除。配置、索引和缓存路径（`/etc/albumctl`、
> `/var/lib/albumctl`）保持不变，可直接沿用；systemd 定时器改为 `nasctl-album-thumbs`。

追番保留下载器的原始文件，在独立媒体库中创建硬链接或副本。文件目录默认只读；显式
启用后支持上传、建目录、复制／跨存储移动、改名、回收恢复和确认后永久清理，
后台任务显示进度并可停止，拒绝覆盖同名目标，见
[`docs/FILES.md`](docs/FILES.md)。媒体库的续播与字幕说明见 [`docs/MEDIA.md`](docs/MEDIA.md)。

## 为什么会有这个项目

这个项目是在一台 NAS 于照片索引任务期间失联之后写的：生成元数据和缩略图时机器
变得无法访问，事故前 `MemAvailable` 只剩约 106MiB。整个设计都是对那次事故的回应：

- **不存在没有上限的任务** —— 每个重任务都必须给出明确的数量和时间上限；
- **同一时间只处理一个媒体文件**，跑在独立进程里；每个文件开始前都过一遍资源
  闸门，叫停时杀掉整个进程组；
- **缩略图不存在时返回占位图**，而不是现场解码 —— 所以打开网页永远不会触发工作；
- **暂停状态能扛住重启**，而且因资源压力停下后**不会自动重启**；
- **不碰** SSH、Tailscale、Samba、已有的 FileBrowser、备份和其他容器。

## 当前状态

原生追番已实现 RSS → 下载 → 归档的单集流程和网页入口，默认关闭自动下载与定时刷新。
以 AutoBangumi 为功能参考，不依赖已删除的 IWantWatchAnime 源码，不包含 Telegram 通知。
配置、API 和功能边界见 [`docs/ANIME.md`](docs/ANIME.md)。文件管理参考 CasaOS 的常用操作。
磁盘、挂载、USB 自动挂载、Samba 共享和网络存储由可选的按需 root 存储助手完成，控制台本身不提权，见
[`docs/STORAGE.md`](docs/STORAGE.md)。
Svelte 前端分为日常使用的用户页面（首页、文件、相册、追番）和管理后台（任务、订阅、相册处理、存储与共享、备份、用户与系统设置），手机使用底部标签栏，见 [`docs/FRONTEND.md`](docs/FRONTEND.md)。
相册默认本地读取，可配置照片目录、限量扫描／缩略图、暂停恢复，也兼容已有远程服务配置，见
[`docs/ALBUM.md`](docs/ALBUM.md)。

Linux amd64、arm64、ARMv7 静态构建通过；本轮完成 Linux/arm64 全量竞态、真实
systemd/cgroup 相册处理、协议下载器闭环、兼容升级回滚及浏览器生成视频验证。
**尚未在目标 NAS 和真实 qBittorrent 上联调。** 下列项目仍未验证：ARMv7 真机
运行行为、目标机上的内存上限、目标机上的 cgroup 是否真能生效、真实媒体树、真实
的看番数据库。最终证据与待验清单见
[`docs/VERIFICATION.md`](docs/VERIFICATION.md)。

## 编译

只需要 Go 1.27.1 或更新版本。其他什么都不需要：不要 cgo、不要交叉编译器、不要
Docker、不要 Node、不要 Python。Svelte 前端的静态产物随源码提交，直接内嵌二进制。
修改前端时才需要 Node 22.12+：`cd frontend && npm ci && npm run check && npm run build`。
视觉方案与开发说明见 [`docs/FRONTEND.md`](docs/FRONTEND.md)。

```sh
go build ./...                       # 两个二进制
go test ./...                        # 完整测试套件需要 Linux 的 /proc 和进程控制

# 可重复地构建 ARMv7 产物
SOURCE_DATE_EPOCH=1760000000 ./scripts/build-armv7.sh
sha256sum -c dist/SHA256SUMS
```

上述脚本的产物是静态链接的 32 位 ARM 二进制，不依赖任何共享库，所以目标机上不需要预装
任何东西。同一个 commit、同一个 `SOURCE_DATE_EPOCH` 构建两次，字节完全一致 ——
这正是校验值有意义的前提，CI 每次推送都会检查这一点。

其他架构可直接交叉编译，例如 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o nasctl ./cmd/nasctl`。
macOS 可用于控制台开发；相册任务的 Linux 资源保护和相关完整测试需要 Linux。

## 运行

控制台和相册命令读同一个配置文件。复制样例再改路径：

```sh
cp configs/nasctl.example.toml /etc/albumctl/config.toml
$EDITOR /etc/albumctl/config.toml

nasctl album doctor --config /etc/albumctl/config.toml    # 相册只读体检
nasctl doctor --config /etc/albumctl/config.toml
```

样例里**每一个策略值都是内置默认值**，所以删掉某一行就等于恢复默认。只有
`[paths]` 和实际使用的媒体／存储目录是本机相关、必须改的；仅使用文件管理或原生追番时可省略相册 collection。
追番的下载与媒体库目录也需按本机配置。键名拼错会**按名字报错拒绝**，
而不是被忽略 —— 这个文件里大多数数字都是上限，悄悄丢掉一个，你就会以为某道限制
在生效，其实没有。

然后，一步一步、手动地来：

```sh
# 控制台；追番是否定时刷新由 hub.anime.auto_poll 控制。
nasctl serve --config /etc/albumctl/config.toml

# 相册任务，有上限，一次一个 collection。
nasctl album scan   --collection photos --limit 500 --dry-run   # 先预览
nasctl album scan   --collection photos --limit 500
nasctl album thumbs --collection photos --limit 200 --max-runtime 20m
```

**默认不做任何定时调度。** 夜间定时器是随包提供的，但处于禁用状态，启用它是
一个需要单独决定、单独授权的步骤。

`nasctl album thumbs` 的退出码是给调度器读的：**0** 完成了这次有上限的工作（包括
"队列本来就是空的"）；**3** 因策略原因提前停下，需要人来判断；**4** 因资源阈值
停下，**不允许循环重试**。有上限的任务返回非零码，含义永远是"没有再往下做"，
绝不是"机器被逼得更狠了"。

## 访问控制

控制台支持管理员、操作者和只读成员，最多 32 个。成员共享已配置文件与媒体，角色
由服务端校验。新部署用 `nasctl init` 创建私有配置，再用 NAS 本机密钥完成首次管理员
设置；基础配置保留 `hub.setup=true`，让状态盘丢失后仍要求认证。绑定公网地址仍会
拒绝启动，除非本机操作者显式放行；远程访问请使用 HTTPS、Tailscale 或 SSH 隧道。

网页会话有效期 12 小时，退出、重启及账号角色／密码变化会撤销会话。`server.token`
继续作为设备管理员 Bearer 凭据，并保留网页登录兼容。普通忘记密码可在本机重置；
账号损坏和设置损坏有保留原件的离线修复入口。
登录与部署说明见 [`docs/AUTH.md`](docs/AUTH.md)，完整功能缺口见
[`docs/ROADMAP.md`](docs/ROADMAP.md)。

AI 调用者可以分配**各自的凭据**（`[[hub.agents]]`）。身份由凭据决定，所以 agent
无法靠"不声明自己是谁"来绕过限制：它的读取会被记进审计日志，除非按 agent 单独
放行否则不能启动重任务，而且它不能给自己批准。

## 明确不做的事

不做 AI 分类、人脸识别、OCR。不做视频实时转码。不做全库持续监听，不做后台无限
循环。相册索引与追番归档保留原文件，不写回 EXIF；文件管理只执行操作者确认的整理。
当前不做网盘上传或云端同步。本地文件备份、校验与拒绝覆盖恢复见
[`docs/BACKUPS.md`](docs/BACKUPS.md)；私有服务状态包、离线恢复与中断恢复入口见
[`docs/STATE-BACKUPS.md`](docs/STATE-BACKUPS.md)。不提供任意 shell；HTTP 文件操作只接受
配置的存储 ID 和相对路径。

## 文档

全新安装、兼容升级与回滚流程见 [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md)。

| 文档 | 内容 |
| --- | --- |
| [`docs/DESIGN.md`](docs/DESIGN.md) | 相册服务：模块边界、数据流、资源预算、格式支持矩阵、明确不实现的功能 |
| [`docs/HUB-AUDIT.md`](docs/HUB-AUDIT.md) | 控制台所依赖的现有代码的审计结果 |
| [`docs/HUB-DESIGN.md`](docs/HUB-DESIGN.md) | 控制台：模块图、UI 草图、权限表、状态机、数据结构、迁移与回滚 |
| [`docs/HUB-STAGE-B.md`](docs/HUB-STAGE-B.md) | 实际交付了什么、发现了哪些缺陷及如何发现、实测资源、以及**所有未验证项** |
| [`docs/HUB-API.md`](docs/HUB-API.md) | 控制台的 HTTP API：每条路由、参数、响应与状态码 |
| [`docs/API.md`](docs/API.md) | 相册服务的 HTTP API |
| [`docs/CLI-ACCESS.md`](docs/CLI-ACCESS.md) | 两个 CLI、权限模型与访问控制 |
| [`docs/RUNBOOK.md`](docs/RUNBOOK.md) | 运维步骤、故障排查、索引恢复、回滚 |
| [`docs/RESOURCE-REPORT.md`](docs/RESOURCE-REPORT.md) | 相册服务的实测资源数据 |

## 关于数字的几条自我约束

这个项目里所有关于资源的说法，都区分三种不同的东西，绝不混用：**进程 RSS**、
**cgroup 内存**、**整机可用内存**。三者互不可替代，`docs/HUB-STAGE-B.md` §5.3
解释了为什么。

模拟器（qemu）下测出的数字**不算目标机实测**，也不会被当作目标机数据陈述。
`HUB-STAGE-B.md` §5.2 记录了一个具体例子：控制台在 x86_64 上满载约 19MB，但在
qemu 下跑到约 47MB，其中大部分几乎确定是 qemu 的 JIT 翻译缓存 —— 而这份报告
明确写了"无法证明其中多少是模拟开销、多少是真实分配"，并据此把 systemd 的内存
上限标为**临时值**，要求上机重测。

## 许可证

MIT，见 [`LICENSE`](LICENSE)。

English: [`README.en.md`](README.en.md)
