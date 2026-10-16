# 原生自动追番

nasctl 的追番模块以 [AutoBangumi](https://github.com/EstrellaXD/Auto_Bangumi)
的 RSS → 下载 → 整理流程为功能参考，使用 Go 原生实现，不需要 Python、旧版
IWantWatchAnime 源码或 suzu 数据库。Telegram 通知不在实现范围。

## 使用

1. 创建独立的下载目录和媒体库目录，给运行 nasctl 的用户读写权限。
2. 在共享配置文件中启用 `[hub.anime]`，填写 qBittorrent WebUI 地址和凭据。
   download_dir 是 nasctl 可访问的本地路径；容器内路径不同可设置
   qbittorrent_download_dir（例如 /downloads/anime），映射到本地 download_dir。
   未设置时两者相同。只接受该前缀及其子目录，不把任意下载器路径转换成本地路径；
   原文件读取仍受已打开的本地下载根目录限制。下载目录和媒体库不能相互包含。
3. 启动 `nasctl serve --config ...`，打开追番页的“连接与调度”，检查 qBittorrent
   地址、目录和连接。连接检查只读取程序/API 版本，不创建下载任务。
   添加订阅时填写名称、RSS、季数与集数范围，可先点击“预览 RSS 匹配”。
4. 点击刷新，核对发现的剧集后选择下载。订阅的“自动下载”开关默认关闭；开启后会
   补全当前 RSS 中所有匹配且尚未入库的单集。聚合 RSS 应配置包含和排除规则。
5. 需要持续追番时设置 `auto_poll = true`，默认每 15 分钟刷新 RSS，每分钟独立
   同步下载进度与归档；没有待处理剧集时不创建空同步任务。暂停模式和
   资源保护会阻止后台任务；已经提交的 qBittorrent 任务由下载器继续管理。

使用随附 systemd 单元时，用 `systemctl edit nasctl-web.service` 为实际目录添加：

```ini
[Service]
ReadWritePaths=/mnt/data/downloads/anime /mnt/data/library/anime
```

随后重载并重启服务。nasctl 和 qBittorrent 用户需要共享读写权限，硬链接还需符合
系统的文件所有权策略；建议使用共同服务用户或正确配置共享组。现有 96 MiB 的服务
内存上限来自旧版控制台，追番启用后的峰值需要在实际机器测量，尚未验证。

RSS 必须是公开 HTTP(S) 地址。Feed 请求拒绝环回、私网和链路本地地址，包括重定向
和实际连接时的 DNS 解析；qBittorrent 则使用配置中明确指定的内网地址。番剧名称
和季数在发现剧集后不能修改，避免已有任务被归档到另一个目录。订阅卡片支持快捷
暂停／启用，也可在编辑表单中操作；不会删除下载器任务或已有媒体文件。快捷操作
只更新调度状态，不会把旧表单覆盖到最新规则上。启用原本已开启自动下载的订阅前，
页面会说明历史补集并要求确认。

当前、已暂停、已归档和全部订阅可以筛选。归档订阅会暂停该订阅的 RSS 发现、下载
核对和媒体归档，但保留订阅 ID、剧集、去重、观看记录和全部文件，也不会停止
qBittorrent 已接受的任务。归档项仍可以查看和播放已入库剧集。恢复后保持暂停，
核对原来的自动下载与范围设置后再明确启用。归档不是删除记录，仍计入 100 个订阅
的总上限；可在已归档卡片中按下述规则导出、清理记录或永久移除订阅。

“起始集数”默认 1，“结束集数”默认 0（不限）。范围参与 RSS 预览和自动发现，
也限制已有“待选择”记录的自动下载；修改范围不会撤销已经排队或提交的任务。
预览最多分析配置上限内的有效条目，展示前 50 条及不匹配原因、已有集数和 RSS
重复发布。预览不保存订阅、不更新检查时间，也不联系下载器；模式与资源保护仍生效。

## 下载和归档

剧集操作可暂停／恢复这一集的下载，或要求 qBittorrent 重新校验文件。操作前重新查找
稳定的单集标签，并核对已保存的任务哈希；不接受浏览器提供的任意哈希，也不提供
“全部任务”或删除下载文件的入口。未找到、重复标签、标识变化和未确认操作均拒绝，
不会借此重新提交下载。全局暂停时仍能暂停下载，恢复和校验继续受模式／资源准入限制。
qBittorrent 4 与 5 的暂停／恢复端点按程序版本选择，依据
[官方 WebUI API](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29)。
HTTP 成功只表示下载器接受请求，后续同步会核对实际状态。暂停／校验状态会保留在
本地剧集记录，重启后可继续核对；不会把暂停任务当作缺失任务重新下载。

- 状态：`discovered → queued → submitted → downloading → archived`。
- “待选择”或从未尝试提交的排队剧集可以“忽略本集”；忽略保留记录并阻止自动
  下载，可显式重新选择下载。只要存在提交历史（包括结果不明的提交），即使手动
  重试后重新排队，也不能当作未下载剧集忽略；已提交任务在下载器中管理。
- 以订阅、季数、集数去重，同一集的不同发布不会自动下载多份。
- 每个任务带稳定的 `nasctl-episode-ID` 标签，重启后先查下载器再决定是否提交。
- 提交前先持久化意图。网络中断等导致提交结果不明确时，不自动再次提交；用户可
  点击“重新核对并排队”。明确拒绝的提交最多重试三次，间隔至少 15 分钟。
- 默认每次最多读取 200 个 RSS 条目、提交 20 个下载、核对 200 个活动剧集；单次
  刷新最多 2 分钟。RSS、下载器响应最大 2 MiB，订阅最多 100 个，剧集最多 20000 条。
- 文件按 `番剧/Season 1/番剧 S01E01.mkv` 整理，并生成单集 NFO。同盘优先硬链接，
  跨盘使用 64 KiB 缓冲复制，单文件最大 8 GiB。原始做种文件保留，不移动或删除。
- 匹配同一视频文件名的 ASS/SRT/VTT 字幕会保留语言后缀一并归档。
- 已有目标文件不会被覆盖；同盘重复归档通过文件身份识别。跨盘复制用临时文件
  发布，并记录来源；遇到无法确认来源的目标文件会报错，等待人工处理。
- 库内视频可直接播放、拖动进度或下载。实际支持的编码取决于浏览器，不执行转码。
- 媒体库支持观看进度与恢复、已看标记、观看筛选、SRT／VTT 字幕选择和已刮削封面。
  存储、权限、上限与验证说明见 [MEDIA.md](MEDIA.md)。
- 剧集列表可按番剧和状态组合筛选。“同步下载进度”只核对下载器和归档，不读取
  RSS 或 TMDB。手动选择下载后直接触发该同步，因此不依赖 RSS 是否仍可用。

开启定时调度后会持续执行任务，因此调度器也会清理超出配置上限的已结束任务历史
和到期审计记录。暂停订阅仍会暂停该订阅的下载核对与归档。

## 元数据

单集 NFO 不需要第三方凭据。设置 `tmdb_token` 并在订阅填写准确的 TMDB **TV 剧集 ID**
后，刷新还会读取中文元数据并保存 `tvshow.nfo`、`poster.jpg`。通过明确 ID 绑定番剧，
不会自动猜测条目。已有元数据文件不覆盖。凭据仅用于 TMDB API，禁止携带凭据重定向。

添加／编辑的“筛选规则与元数据”中可按名称搜索 TMDB TV 剧集，查看中文／原名、
首播日期和简介，再明确选择条目与季数。使用官方 [TV 搜索](https://developer.themoviedb.org/reference/search-tv)
与 [剧集详情](https://developer.themoviedb.org/reference/tv-series-details) 接口。选择只修改
表单的 TMDB ID 和你指定的季数，保留名称、RSS 与规则；保存订阅后才持久化。
已有剧集时的名称／季数限制仍然生效，现有 NFO／封面也不会因换 ID 被覆盖。

“更新元数据”对已有媒体库执行有界任务：默认补齐缺失项；明确勾选后可替换 NFO／封面。
每批 1–50 集、最长 2 分钟，返回继续位置，可接着处理下一批。未配置 TMDB 时生成基础
NFO；配置了明确 TV ID 时读取剧集和对应季／集的元数据。视频、字幕、观看状态保持原样。
自动刮削和手动更新共用受限请求，JSON 最多 512 KiB／12 秒，封面最多 2 MiB。

每次替换会先在私有状态目录保存独立旧版本（含字节校验），再发布新文件。旧版本总量
最多 1000 个、64 MiB；达到预算时拒绝替换，保留当前文件。连接与调度页可下载、恢复
或确认后永久清理指定版本。恢复也先保存当前版本；校验不符、符号链接、超限文件和
不再属于现有订阅／媒体库的目标都拒绝。已移除订阅的元数据备份仍可下载，自动恢复
需要匹配现有订阅或入库记录。封面版本随文件更新变化，页面刷新会读取新封面。

## 历史导出与清理

订阅卡片和连接与调度页支持导出 NDJSON。独立只读 SQLite 快照包含订阅、剧集、GUID、
观看状态与去重标记，不占用主写连接；不包含视频、下载器账号或 TMDB 令牌。RSS／发布
地址仍在导出中。明确上限 1–20000 条剧集，去重标记上限 200000 条，最大 512 MiB、
2 分钟，使用最多 3 个共享传输槽和 30 秒写入空闲期限。最后一行 kind=end、complete=true
才表示文件完整；truncated=true 表示仅导出了指定数量。超限或失败不会伪装完整导出。

清理要求先归档订阅、输入完整名称并确认。普通清理每次最多 1000 条，可设置截止日期；
保留最小去重标记，所以恢复订阅后不会把已经清理的同一集再次发现／自动下载。
默认保留可能关联下载器的记录；明确勾选解除关联才允许移除这些记录，下载器任务
仍继续运行，nasctl 不再跟踪。视频、字幕和 NFO 不删除。

永久移除整个订阅须覆盖其全部记录，数量上限 20000、最长 30 秒；同时删除该订阅的
去重标记并释放 100 订阅限额。订阅／剧集编号使用持久单调计数器，清理最高编号后也不
复用，避免旧下载标签被新记录接管。普通清理后的去重标记总量上限 200000 条。
清理释放的 SQLite 页会复用，不在请求中执行重写整个库的压缩。

查询最多 200 字、20 页，每页显示至多 20 项；达到上限会提示。季信息限第 1–99 季，
不把特别篇当正常季选择。单次 HTTP 请求最多 12 秒、512 KiB，查询串行执行，关闭
表单会取消请求。查询不修改订阅、不读取 RSS、不提交下载，也不下载图片或写媒体库。
所有凭据留在后端且仅发往固定的 TMDB API 域名。未配置凭据时仍可手填 ID；网络、
授权、频率或响应错误会提示，不自动猜测或替换当前绑定。

## HTTP API

| 路由 | 用途 |
| --- | --- |
| `GET /api/v1/anime/status` | 启用状态、刷新状态和各阶段数量 |
| `GET /api/v1/anime/settings` | 当前启动配置摘要，不返回用户名、密码或 TMDB 凭据 |
| `GET /api/v1/anime/metadata/search?q=名称&page=1` | 操作者按名称查询 TMDB，返回 results/page/has_more/truncated |
| `GET /api/v1/anime/metadata/series/{id}` | 操作者读取剧集及第 1–99 季信息 |
| `POST /api/v1/anime/subscriptions/{id}/metadata` | 已有媒体元数据更新，limit/after_id/replace；需确认头，返回后台任务 |
| `GET /api/v1/anime/metadata/backups` | 操作者读取受限的元数据旧版本列表 |
| `GET /api/v1/anime/metadata/backups/{id}/download` | 下载校验通过的旧版本字节 |
| `POST /api/v1/anime/metadata/backups/{id}/restore` | 恢复所选旧版本；需确认头，返回后台任务 |
| `DELETE /api/v1/anime/metadata/backups/{id}` | 清理指定旧版本；需确认头和匹配 sha256 |
| `GET /api/v1/anime/history/export?limit=20000&subscription=ID` | 一致只读快照导出；省略 subscription 导出全部 |
| `GET /api/v1/anime/subscriptions/{id}/history` | 清理前的记录／去重／关联数量 |
| `POST /api/v1/anime/subscriptions/{id}/history/cleanup` | 有界历史清理；需确认头、完整名称及限额 |
| `POST /api/v1/anime/subscriptions/{id}/enabled` | `{"enabled":true或false}`，仅更新调度状态 |
| `POST /api/v1/anime/subscriptions/{id}/archive` | `{"archived":true或false}`，可恢复；恢复保持暂停 |
| `POST /api/v1/anime/connection` | 检查配置中的 qBittorrent 程序和 API 版本 |
| `POST /api/v1/anime/preview` | 评估未保存的订阅 JSON，返回匹配统计与原因 |
| `GET /api/v1/anime/subscriptions` | 列出订阅 |
| `POST /api/v1/anime/subscriptions` | 创建订阅 |
| `PUT /api/v1/anime/subscriptions/{id}` | 编辑或暂停订阅 |
| `GET /api/v1/anime/episodes?subscription=ID&state=&watch=&limit=50&offset=0` | 按订阅、下载／观看状态分页查询剧集 |
| `POST /api/v1/anime/refresh?subscription=ID` | 后台刷新；省略 ID 则刷新全部，返回任务 ID |
| `POST /api/v1/anime/sync?subscription=ID` | 后台同步下载与归档，不读取 RSS |
| `POST /api/v1/anime/episodes/{id}/queue` | 手动选择下载或显式重试 |
| `POST /api/v1/anime/episodes/{id}/download` | 单集下载暂停／恢复／校验；action=pause/resume/recheck，需确认头 |
| `POST /api/v1/anime/episodes/{id}/ignore` | 忽略尚未尝试提交的剧集，不操作下载器或文件 |
| `GET /api/v1/anime/episodes/{id}/video` | 播放入库视频，支持 HTTP Range |

所有写入只允许操作者身份，AI 凭据只能读取。刷新任务进入统一任务记录；订阅和
手动排队写入审计。设置了 `server.token` 时 API 需要 Bearer 凭据或已有的
浏览器会话 Cookie。网页登录、退出和有效期见 [AUTH.md](AUTH.md)；旧 nasctl_token
Cookie 保留兼容，新登录不再向浏览器保存原始 API 令牌。

订阅 JSON 新增 `start_episode` 和 `end_episode`。预览与连接检查仅限操作者，
只读控制台可执行这些不写状态的检查；刷新、同步、订阅修改和忽略仍被只读配置禁止。
订阅编号无效时直接拒绝，不会意外回退为操作全部订阅。

qBittorrent 连接检查使用官方 [WebUI API](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29)
的 `app/version` 与 `app/webapiVersion`，复用 Cookie 登录流程。

## 状态升级

已有原生 `anime.sqlite` 会在事务中升级至 schema 5，保留订阅、剧集、下载和归档记录，
补充集数范围、永久的“曾尝试提交”标记、观看状态和订阅归档时间。旧订阅默认未归档，
不会改变启用状态，重复启动不会重复增加字段。旧版排队记录
缺少完整提交历史，迁移时保守地视为可能已提交，不允许直接忽略；仍可核对和显式重试。
schema 5 添加清理去重表和持久编号计数器；没有清理指令时不移除现有数据。旧版程序不
读取新 schema，升级前应保存一致数据库备份，回滚时使用对应状态副本。

## 当前边界

本阶段支持 RSS 单集、qBittorrent 和明确 ID 的 TMDB 元数据。尚未支持自动识别新番
并创建订阅、按质量替换已下载集数、合集拆分、特殊篇/电影分类、aria2 和多用户账户。
旧 suzu 只读页面作为可选兼容入口保留，不自动迁移。

文件管理器以 CasaOS 为功能参考，已接入可写根目录的常用整理操作，见 [FILES.md](FILES.md)。

## 本轮验证

在 macOS 开发机使用 Go 1.27.2 验证：

新增竞态／真实 HTTP 测试覆盖容器路径映射、qBittorrent 4/5 单集控制、暂停模式停止
任务、元数据分批与恢复字节、备份预算／越界、历史清理与去重、编号不复用和并行只读
导出快照。真实 nasctl＋编译前端的 download-control、metadata、history DOM 流程通过。
下载器为明确标记的本机协议夹具，不代表已连接实际 qBittorrent 账号或完成真实下载。

- `go test -race -count=1 ./internal/anime ./internal/hubweb ./internal/config ./internal/hubops ./internal/nasctl` 通过。
- `go vet ./...`、JavaScript 语法检查和差异空白检查通过。
- 两个二进制均以 `CGO_ENABLED=0` 构建了 Linux amd64、arm64、ARMv7 版本。
- 隔离的浏览器测试确认可添加、编辑并暂停订阅；没有提交真实下载任务。
- 完整 `go test ./...` 在 macOS 上仍有既有 Linux 专用测试失败：相册 e2e 需要
  `/proc`/cgroup 资源信号，resource 测试需要 Linux 进程组和僵尸进程行为。

追番流程使用模拟 RSS、qBittorrent HTTP API 和下载器验证，覆盖重启去重、提交结果
不明时不重复下载、拒绝后的限额与退避、路径边界和归档不覆盖。真实下载器兼容性、
跨盘大文件复制、目标 NAS 的运行内存和 CPU 尚未实测；交叉编译不代表真机验证。

新增测试覆盖预览无写入、规则与发现结果一致、集数范围、忽略与恢复、提交历史不因
重试丢失、独立同步不访问 RSS、旧 schema 保留数据升级、空队列检测、订阅筛选、
连接检查 Cookie 登录、凭据不出现在设置响应，以及操作者/AI/只读权限边界。
DOM 流程中的预览结果使用受控响应验证展示；真实 RSS 读取与规则计算由后端测试验证。

元数据测试验证固定域名与凭据、重定向拒绝、查询／响应限额、结果去重、季数过滤、
错误不泄露凭据及并发与取消；没有使用真实 TMDB 凭据。订阅管理测试覆盖归档后调度
停止、保留去重与观看状态、重开数据库、旧 schema 升级与恢复暂停。DOM＋真实 Go
API 验证元数据选择保存、暂停、筛选、归档恢复和启用自动下载前的确认；DOM 中的
TMDB 响应为测试数据，服务端网络行为由独立测试覆盖。
实际浏览器另检查订阅卡片、归档确认窗口，以及未配置 TMDB 时的手填 ID 提示。
本地主预览升级前已备份，并按原字段核对订阅、剧集与观看记录，schema 4 升级未改变
原有数据。真实 TMDB 搜索、真实下载器与 NAS 仍待部署联调。

## 搜索番剧并选择订阅源

“添加订阅”支持蜜柑 Mikan 和 Bangumi 检索，无需配置 TMDB 凭据。直接搜 Mikan 后，选择番剧、字幕组并点击“使用这个订阅源”；名称和字幕组 RSS 会填入草稿。Bangumi 提供番剧资料，不提供下载 RSS：选择 Bangumi 条目后会搜索 Mikan，再由用户核对具体来源。如果 Mikan 明确关联了另一个 Bangumi 编号，禁止应用，避免选错季。

搜索和选源不会保存订阅、提交下载或刷新 RSS。确认季数、起止集数及清晰度/语言规则后，才点击“添加订阅”；自动下载默认关闭。每部番剧建议只选一个字幕组，“全部字幕组”可能包含重复发布。选定的单番 RSS 默认使用 `.+` 包含规则，避免资料名称和字幕组标题的简繁/日文差异导致漏匹配；可自行缩小规则。

保留手动 RSS 和原有 TMDB 刮削功能。搜索结果封面仅用于搜索展示，当前入库元数据和本地封面仍沿用 TMDB，不会把 Bangumi 编号当作 TMDB ID。搜索最多显示 12 个结果，Bangumi 最多翻 10 页；Mikan 只读取番剧卡片列表，提前关闭包含大量历史种子的响应，字幕组最多列出 64 个。固定公共来源、15 秒超时、有界响应和单并发查询；不绕过站点验证、限流或网络访问限制。

接口参考：[Bangumi 官方 API](https://github.com/bangumi/api/blob/master/open-api/v0.yaml)、[Mikan](https://mikanani.me/)。
