# 控制台中的相册

相册作为桌面应用，默认直接读取本地索引、缩略图与原文件。只启动 nasctl 即可，
无需独立相册 HTTP 服务。索引复用现有 albumctl 格式，以只读 SQLite 池读取；浏览
不会扫描、解码、修复索引或生成缩略图。已有远程相册配置仍然兼容。

## 本地配置与处理

`paths.index_file`（默认 state_dir 下的 index.sqlite3）、`paths.cache_dir` 和
`[[collections]]` 继续有效。空 API 地址默认本地模式；可显式设置：

```toml
[hub.album]
mode = "local"
# worker_binary = "/usr/local/bin/albumctl"
```

在“管理相册”中从已配置的存储位置添加相对目录，可编辑名称、启用状态和同卷许可。
最多 64 个托管目录、配置总量 64 KiB，以 0600 原子保存于 state_dir/album-directories.json，
不改写主配置。主配置中的集合继续由主配置管理。新配置立即作用于后续请求，不打断
正在读取的原图；移除配置隐藏相应列表／详情／缩略图，不删除原文件与索引行。
托管目录原文件通过已打开的 NAS 根目录读取，目录被替换成越界符号链接时仍拒绝访问。

首次安装缺少索引时显示建立索引入口，读取不会创建空数据库。后台处理建立索引后，
下次读取自动连接。损坏索引显示不可用，后台不会擅自重建或覆盖。

将 albumctl 处理组件安装在 nasctl 同目录，或设置绝对 worker_binary 路径。
它只在请求处理任务时启动，无需手工运行服务。扫描／缩略图每次必须指定 1–10000 项
和 1 秒至 2 小时上限；同一时间一个本地相册处理任务。缩略图继续使用原组件的逐文件
进程隔离／cgroup／内存与挂载检查，网页不提供跳过 cgroup 或强制解除资源冷却的入口。
可暂停后续处理、恢复许可，恢复不会自动开始扫描。当前任务可在任务中心停止。
取消先通知本次启动的处理进程组收尾，最多等待 15 秒后终止主进程；异常强杀或主服务
崩溃后的孤立进程只报告，不自动接管或杀死从旧 PID 记录猜测出的进程。

Linux 由 systemd 的 `Delegate=yes` 和可写的本服务 cgroup 子树提供隔离。NAS 先移到
自己的 supervisor 子组，让单次 albumctl 处理进程在独立 task 子组等待私有启动门；
确认已进入该组后才准许加载配置和处理。albumctl 再为解码器建立自己的受限子组。
这样满足 [cgroup v2 的内部进程约束](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html#no-internal-process-constraint)，不会把 NAS 本身误当成解码器一起限额。
退出后只清理本次创建的已知空子组，不递归删除或杀死其他组；残留活跃进程时拒绝复用。
随附单元的总上限为暂定 256 MiB，包含 NAS、处理监督进程和解码器，默认解码器仍为
128 MiB，上层单次处理组增加 64 MiB 监督预算。总上限是硬界限，不能当成实际 RAM 用量；
部署后须测量并按自己的配置校正。浏览与扫描不因读取页面而建立解码处理组。

所有写入口要求操作者、可写控制台、明确确认、任务准入与审计。配置在执行任务时不可
修改；配置临时快照不含 NAS 用户、AI 或下载器凭据，任务结束删除。处理日志只保存
最后 4 KiB，进度来自该次处理 PID 的任务记录，不编造进度；没有进度记录时显示总量未知。
设置 mode="disabled" 停用相册。

## 远程兼容配置

先配置并启动独立服务，例如 `albumctl serve --config /etc/albumctl/config.toml`。
将其实际监听地址和凭据填入 nasctl 的配置文件，重启 nasctl 后生效：

```toml
[hub.album]
api_base = "http://127.0.0.1:8088"
token = "相册服务的现有令牌"
# public_url = "https://your-album.example" # 可选，只用于独立服务的浏览器入口
```

8088 是示例端口，必须与 albumctl 的 server.bind 一致。API 地址只接受 HTTP(S)、
localhost 或字面量私网／回环 IP；支持干净的路径前缀。不接受用户名、密码、查询参数、
fragment、公网地址或任意 DNS 主机名；容器可将端口发布到回环地址。
请求不使用系统代理，也不跟随重定向。localhost 的拨号固定为 127.0.0.1。

`token` 是独立服务的凭据，仅由后端使用；不会继承 nasctl 的操作者令牌，也不向
albumctl 转发浏览器 Authorization、Cookie。独立服务未设令牌时可省略此项。
`public_url` 不影响代理请求；打开独立入口仍按该服务自身的访问控制处理。
显式 api_base 自动选择 remote 模式，也可以设置 mode="remote"。连接失败显示实际不可用状态。上游认证失败映射为
502，不会使有效的 nasctl 会话退出。相册代理同样受 nasctl 身份验证和 AI 读取审计约束。

## 已有功能

- 集合选择、文件名／目录搜索、类型、起止日期、排序；可显示缺失原文件、估计时间和 GPS 项。
- 每页 48 项，保留最近 100 页的返回位置；未应用的筛选修改不影响当前翻页。
- 缩略图画廊、文件详情、相机／尺寸／位置／EXIF 信息。缺失原文件仍可看已有缩略图。
- 照片默认只读缩略图，点击“查看原图”才读取原文件；原文件下载为独立附件。
- 常见视频容器交给浏览器播放；保留单段 Range／HEAD／缓存验证，不提供在线转码。
  容器可播放不代表其中编码一定受浏览器支持，失败时可下载使用本地播放器。
- 拍摄时间按相册记录的墙钟时间展示，不擅自转换不带时区的相机时间；估计时间有明确标识。

## HTTP 与资源边界

所有接口都在 `/api/v1/album` 下：

| GET 接口 | 内容 |
| --- | --- |
| `/status` | configured、connected、mode、index_ready、版本、暂停状态、安全错误摘要、可选 public_url |
| `/collections` | 已配置集合及索引数量 |
| `/media` | 筛选与游标分页；limit 默认 48、上限 100 |
| `/media/{id}` | 媒体详情 |
| `/media/{id}/thumb` | 已有缩略图／占位图；最多 2 MiB、10 秒 |
| `/media/{id}/raw` | 原文件，支持单段 Range 和 HEAD |
| `/media/{id}/download` | 原文件附件下载，支持 HEAD |

查询参数为 collection、q、kind、from、to、sort、cursor、limit、include_missing、
fallback、gps。响应中的媒体 URL 重写为同源地址，页面不使用上游返回的外部链接。
JSON 响应限制 2 MiB／10 秒；缩略图禁止 HTML 等非图片类型，SVG 占位图通过 sandbox
响应策略隔离直接导航。原文件要求明确大小，64 KiB 缓冲、30 分钟总时长和 30 秒读写
空闲上限；与其他媒体传输共享最多 3 个流槽，缩略图受全局并发上限约束。

本地管理接口为 GET `/settings`、POST `/directories`、DELETE `/directories/{code}`，
以及 POST `/tasks/{scan,thumbs,pause,resume}`。写入需要 X-Nasctl-Confirm:1；任务返回
job_id，进度与停止共用 `/api/v1/jobs`。元数据编辑尚未提供。

## 验证

Go 测试验证配置、凭据隔离、响应限额、重定向拒绝、身份验证、Range／HEAD／304／416
和缩略图内容隔离；新增本地读取、配置热更新、拒绝越界、后台凭据隔离与取消竞态验证。
`album-native.mjs` 验证真实 nasctl 的目录 UI 与按需启动真实 albumctl 的暂停／恢复，
无需相册 HTTP 监听。`scripts/uitest/seed-album` 仅用于开发机／CI，要求命名的空目录或
带所有权标记的测试目录及 `--fixture-only`，生成 57 条合成索引和图片；两个真实服务加
`album.mjs` 验证分页、过滤、详情、缺失项及原文件字节。视频记录仅验证 HTTP／DOM
流程，不能作为实际编码播放或真实 NAS 资源验证。

## 照片浏览与保险箱

相册采用照片网格和按月时间线，提供“全部照片 / 视频 / 相簿”；筛选收起，照片信息在大图窗口按需展开。支持键盘左右切换本页照片、下载原图，保持每页 48 项的边界。打开一张照片才读取其原图，普通网格仅使用已有缩略图。

本地相册新增独立密码的[照片保险箱](PHOTO-VAULT.md)，加密原件、缩略图和元数据；请先阅读该文档的存储、备份和中断处理说明。
