# 升级与迁移

本文按版本记录需要手工处理的变化。通用的检查点、升级和回滚原则见
[DEPLOYMENT.md](DEPLOYMENT.md#升级与检查点)，状态包细节见 [STATE-BACKUPS.md](STATE-BACKUPS.md)。

跨多个版本升级时，从当前版本开始按顺序阅读各节；可以一次直接安装最新程序，但要完成
途经各节里的手工步骤。例如 0.2.x 升级到 0.6.0：先按“0.2.x / 0.1.x → 0.3.0”处理 albumctl
合并和配置，再依次看后面各节。

| 当前版本 | 需要阅读 |
| --- | --- |
| 0.7.x | [0.7.x → 0.8.0](#07x--080)：替换程序即可；想用存储与共享再装几个可选文件 |
| 0.6.x | 替换程序即可（0.7.0 是新的桌面式首页，没有配置或数据变化），再看 [0.7.x → 0.8.0](#07x--080) |
| 0.5.x | [0.5.x → 0.6.0](#05x--060)（同样适用于升级到 0.6.1、0.7.0） |
| 0.4.x | [0.4.x → 0.5.0](#04x--050)，然后 [0.5.x → 0.6.0](#05x--060) |
| 0.3.x | [0.3.x → 0.4.0](#03x--040)，然后依次阅读后面各节 |
| 0.2.x、0.1.x | [0.2.x / 0.1.x → 0.3.0](#02x--01x--030)，然后依次阅读后面各节 |

## 0.7.x → 0.8.0

这一版**没有配置或数据格式变化**。只替换程序时，新的“存储与共享”页面会显示启用方法，
其他功能不变；回滚也只需换回旧程序。

### 变化一览

| 变化 | 影响 | 需要做什么 |
| --- | --- | --- |
| 新增“设置 › 存储与共享” | 磁盘与健康状态、挂载／卸载／开机挂载、格式化、USB 自动挂载、Samba 共享、SMB／NFS 网络存储 | 想用时按 [STORAGE.md](STORAGE.md#启用) 安装存储助手（一个 socket 和三个 systemd 单元、一条 udev 规则、一个 tmpfiles 配置） |
| `nasctl-web.service` 增加 `ReadWritePaths=-/media/nasctl` | 控制台可以写入助手挂载的磁盘（仍需在存储位置里打开“允许文件整理”） | 用存储助手时更新单元文件，或在 drop-in 里加这一行 |
| 新命令 `nasctl storage-helper` | 只给 `nasctl-storage@.service` 和自动挂载单元使用 | 无 |
| 新增 `/api/v1/storage` 系列接口 | 仅管理员 | 无 |

### 升级步骤

校验安装包，保留旧程序，建立状态检查点，替换程序并重启，`nasctl version` 应显示 0.8.0。
不使用存储助手时 systemd 单元不需要更新。

### 回滚

换回 0.7.x 程序即可。启用过存储助手的话，先 `systemctl disable --now nasctl-storage.socket`
（旧程序没有 `storage-helper` 命令）。已经挂载的磁盘和写进 `/etc/fstab` 区块的开机挂载、
Samba 共享会保留，需要时手工删除，见 [STORAGE.md](STORAGE.md#文件位置)。

## 0.5.x → 0.6.0

这一版**没有配置或数据格式变化**，替换程序并重启即可，回滚也只需换回旧程序。

### 变化一览

| 变化 | 影响 | 需要做什么 |
| --- | --- | --- |
| 界面改为仿 Nextcloud 的布局 | 所有页面共用蓝色顶栏；管理后台和“我的”合并为“设置”，左侧分“个人”“管理”；地址不变 | 无；浏览器刷新即可 |
| 修复从头像菜单进入管理后台后整页无法点击 | — | 无 |
| 相册处理改为整目录分批 | “处理相册”一次处理完整个目录（扫描每批 500、缩略图每批 50），运行时间最长 12 小时；不再需要填写“每次最多处理” | 无；之前只扫到一部分的目录，重新点“扫描并生成缩略图”即可继续 |
| `nasctl album scan --limit` 的计数方式 | 只计算新增、变化或推迟的文件，重复执行会逐步扫完整个目录；还有剩余时输出末尾多一行 `nasctl-batch: more` | 自己写的定时脚本无需修改，退出码不变 |
| WebDAV 下载跟随跳转 | 123 云盘官方 WebDAV、AList“302 重定向”等可以通过回读校验和恢复；账号密码不会发给跳转目标；写操作仍不跟随跳转 | 之前因为“回读校验 302”失败的备份，重新运行即可 |
| 大照片也生成缩略图 | JPEG 按缩略图需要的比例缩小解码，`max_pixels` 和解码预算按解码后的尺寸计算；之前因“超出像素预算”被跳过的照片，下次扫描时自动重新排队 | 点一次“扫描并生成缩略图” |
| 缩略图按 EXIF 方向转正 | 手机竖拍照片的缩略图不再横躺；已有的缩略图不会自动重做 | 可选：在 `cache_dir` 删除缩略图后重新生成 |
| 可选 libvips | 装了 `libvips-tools` 后，PNG、WebP、TIFF、HEIC、AVIF 由它生成缩略图（HEIC 还需要 `libheif-plugin-libde265`） | 想要 HEIC 缩略图就安装；新配置项 `formats.use_vips`、`vips_path`、`vips_for_jpeg` 有默认值，不用写 |
| 缩略图并行 | 4 核及以上默认同时 2 个处理程序，共用处理程序内存上限 | 可用 `decode.thumb_workers` 调整（0 自动，最多 4） |
| `album.scan`、`album.thumbs` 任务时限 | API 的 `max_runtime` 上限从 2 小时提高到 12 小时，`limit` 改为每批数量 | 调用 API 的脚本无需修改 |

### 升级步骤

与前面各节相同：校验安装包，保留旧程序，建立状态检查点，替换程序并启动，
`nasctl version` 应显示 0.6.0。systemd 单元不需要更新。

### 验证

- 浏览器刷新后，页面顶部是蓝色顶栏，点右上角“管理”进入设置，左侧有“个人”和“管理”两组导航；
- 从头像菜单点“管理设置”，进入后仍能正常点击左侧导航；
- 在“相册管理 › 处理相册”选一个目录点“扫描并生成缩略图”，任务中心能看到“第 N 批”的进度；
- 用 123 云盘或 AList 做 WebDAV 备份的，到“管理 › 备份”重新运行一次。

### 回滚到 0.5.x

换回旧程序即可，数据不需要处理。注意 0.5.x 的相册处理仍是“每次最多处理 N 个、没有变化的
文件也计入”，大照片也会重新被跳过（已生成的缩略图保留）。如果在配置里写了
`use_vips`、`vips_path`、`vips_for_jpeg` 或 `thumb_workers`，回滚前要删掉，否则 0.5.x 会因为
未知配置项拒绝启动。

## 0.4.x → 0.5.0

这一版**没有必须手工处理的配置或数据变化**，替换程序并重启即可。新功能“私密目录”默认不启用。

### 变化一览

| 变化 | 影响 | 需要做什么 |
| --- | --- | --- |
| 新增私密目录 | 相册目录可设为私密：文件留在原处，解锁保险箱后才能在相册和文件页看到 | 想用时按 [PHOTO-VAULT.md](PHOTO-VAULT.md#私密目录) 设置；不用则无影响 |
| 主配置 `[[collections]]` 新增可选键 `private` | 默认 `false` | 无；私密目录不能与普通目录互相包含，设置时会检查 |
| 保险箱会话改为“闲置 15 分钟”自动锁定 | 查看私密内容会重新计时，看长视频不会中途锁定 | 无 |
| 保险箱不再因切换页面而锁定 | 解锁后可以在相册、文件页之间切换；标签页隐藏超过 2 分钟、关闭或刷新页面仍会锁定；右上角“已解锁”按钮可随时锁定 | 无 |
| 保险箱 Cookie 范围从 `/api/v1/album/vault` 扩大到 `/api/v1` | 文件页也能识别解锁状态；仍是 HttpOnly、SameSite=Strict 的会话 Cookie | 无；旧 Cookie 会在下次解锁或锁定时自动清除 |
| 修改、移除私密目录需要解锁保险箱 | 操作者等能管理相册的账号无法关掉保护 | 无 |
| 新增 API `/api/v1/album/private/…` | 只在解锁后可用；文件 API 遇到私密目录返回 `403 vault_locked` 或 `409 private_folder` | 自己写的脚本如果遍历文件，请处理这两个错误代码 |
| 系统设置、相册管理、保险箱和“我的”页面改版 | 地址不变；设置项和保存方式（校验后保存、重启生效）不变 | 无 |

配置、账号、状态库、相册索引（schema 2）和追番库（schema 5）都没有改变。设置了私密目录后，
`state_dir/album-directories.json` 中对应条目会多出 `"private": true`。

### 升级步骤

与 0.3.x → 0.4.0 相同：校验安装包，保留旧程序，建立状态检查点，替换程序并启动
（把下面示例里的版本号换成 0.5.0）。systemd 单元不需要更新。

```sh
mkdir -p ~/nasctl-0.4-rollback
sudo cp -a /usr/local/bin/nasctl ~/nasctl-0.4-rollback/
sudo systemctl stop nasctl-web.service
sudo /usr/local/bin/nasctl --config /etc/albumctl/config.toml state backup \
  --package /mnt/backup-private/nasctl-state/before-0.5.0 --confirm
sudo install -m 755 ./nasctl /usr/local/bin/nasctl.new
sudo -u nasctl /usr/local/bin/nasctl.new --config /etc/albumctl/config.toml doctor
sudo mv /usr/local/bin/nasctl.new /usr/local/bin/nasctl
sudo systemctl start nasctl-web.service
nasctl version   # 应显示 nasctl 0.5.0
```

### 验证

- 浏览器刷新后，“管理后台 › 系统设置”显示为分区卡片，左侧有分区导航；
- 已在用照片保险箱的，到“相册 › 保险箱”解锁，确认加密照片仍在“加密照片”中；
- 想试用私密目录：建一个测试文件夹，在相册管理中添加为私密目录，锁定后确认它在文件页消失，
  解锁后带锁形标记出现。

### 回滚到 0.4.x

```sh
sudo systemctl stop nasctl-web.service
sudo cp -a ~/nasctl-0.4-rollback/nasctl /usr/local/bin/nasctl
sudo systemctl start nasctl-web.service
```

**设置过私密目录时，回滚前要先取消私密。** 0.4.x 不认识 `"private"`，读到它会拒绝加载相册
目录配置；即使能加载，0.4.x 也不会隐藏这些目录。请在 0.5.0 中解锁保险箱，到相册管理把每个
私密目录的“设为私密目录”关掉并保存，再回滚。主配置文件里加过 `private = …` 的，也要先删掉
这一行，否则 0.4.x 会因为未知配置项拒绝启动。没有用过私密目录则无需处理。

## 0.3.x → 0.4.0

这一版**没有不兼容的配置或数据变化**，替换程序并重启即可。下面列出需要留意的行为变化。

### 变化一览

| 变化 | 影响 | 需要做什么 |
| --- | --- | --- |
| 大文件传输改为“30 秒无进展才断开” | 超过 60 秒的下载、视频播放不再被 `server.write_timeout` 切断 | 无；如果前面有反向代理，见下文“反向代理” |
| 新增文件夹打包下载 `GET /api/v1/files/archive` | 下载时长取决于文件夹大小，占用一个传输名额 | 无；上限沿用 `hub.files` 的 `copy_max_entries` 和 `copy_max_bytes` |
| “保持登录 30 天” | 勾选后会话写入 `state_dir/remembered-sessions.json` | 无；不想提供这个选项时见下文 |
| API 报错改为中文 | `message` 字段变为中文，英文原文移到 `detail` | 自己写的脚本请改为判断 `error` 字段 |
| WebDAV 目录请求带末尾 `/` | 兼容 nginx 的 WebDAV 模块 | 无；个别服务端若因此报错，详情里会显示具体请求 |
| 新增公开的应用清单和图标 | `/manifest.webmanifest`、`/icon-*.png`、`/apple-touch-icon.png` 无需登录 | 无；内容不含私有信息 |

配置、账号、状态库、相册索引（schema 2）和追番库（schema 5）都没有改变。

### 升级步骤

**1. 下载并校验**（以 arm64 为例）

```sh
sha256sum -c nasctl-linux-arm64.tar.gz.sha256
tar -xzf nasctl-linux-arm64.tar.gz
cd nasctl-linux-arm64
sha256sum -c SHA256SUMS
```

**2. 保留旧程序，建立检查点**

```sh
mkdir -p ~/nasctl-0.3-rollback
sudo cp -a /usr/local/bin/nasctl ~/nasctl-0.3-rollback/
sudo systemctl stop nasctl-web.service
sudo /usr/local/bin/nasctl --config /etc/albumctl/config.toml state backup \
  --package /mnt/backup-private/nasctl-state/before-0.4.0 --confirm
sudo /usr/local/bin/nasctl --config /etc/albumctl/config.toml --json state verify \
  --package /mnt/backup-private/nasctl-state/before-0.4.0
```

如果在用照片保险箱，状态包不包含保险箱照片，请按 [PHOTO-VAULT.md](PHOTO-VAULT.md#存储与备份)
另行备份。

**3. 替换程序并启动**

```sh
sudo install -m 755 ./nasctl /usr/local/bin/nasctl.new
sudo -u nasctl /usr/local/bin/nasctl.new --config /etc/albumctl/config.toml doctor
sudo mv /usr/local/bin/nasctl.new /usr/local/bin/nasctl
sudo systemctl start nasctl-web.service
nasctl version   # 应显示 nasctl 0.4.0
```

systemd 单元与 0.3.0 相同，不需要更新。

**4. 验证**

- 浏览器刷新后显示新界面（右上角头像点开是菜单），无需清理缓存；
- 选中一个小文件夹点“打包下载”，得到可以打开的 ZIP；
- 下载一个需要超过一分钟的大文件，确认中途不再中断；
- 如果用 WebDAV 备份，到“管理后台 → 备份”点“测试连接”。

### 反向代理

如果通过 nginx、Caddy 等反向代理访问 nasctl，长时间下载和文件夹打包还受代理自身超时和缓冲
限制。以 nginx 为例，建议对 nasctl 的站点设置：

```nginx
proxy_read_timeout 300s;     # 打包大文件夹时第一个字节前可能需要扫描目录
proxy_buffering off;         # 边读边发，避免代理把整个文件缓存到磁盘
client_max_body_size 0;      # 上传大小由 nasctl 的 upload_max_bytes 控制
```

### “保持登录”

登录页的“保持登录 30 天”默认不勾选。勾选后，只有 Cookie 的 SHA-256 摘要、账号 ID、账号版本
和到期时间写入 `state_dir/remembered-sessions.json`，状态备份不包含它。需要让所有人立即重新
登录时，停止服务后删除这个文件再启动。改密码、改角色、停用或删除账号会自动让相关会话失效。

### 回滚到 0.3.x

```sh
sudo systemctl stop nasctl-web.service
sudo cp -a ~/nasctl-0.3-rollback/nasctl /usr/local/bin/nasctl
sudo systemctl start nasctl-web.service
```

0.3.x 不认识 `remembered-sessions.json`，会忽略它，所有人需要重新登录；再次升级到 0.4.0 后，
未过期的保持登录会话会恢复。其他数据无需处理。

## 0.2.x / 0.1.x → 0.3.0

0.2.0 与 0.1.x 的迁移步骤相同；文中的“旧版”指当前运行的 0.2.x 或 0.1.x。

### 变化一览

| 变化 | 影响 | 需要做什么 |
| --- | --- | --- |
| `albumctl` 并入 `nasctl album` | 安装包只剩一个 `nasctl` 二进制 | 删除旧 `albumctl`，脚本里的 `albumctl …` 改为 `nasctl album …` |
| 移除 `albumctl serve` 独立相册网页 | 8088 等独立相册端口不再监听 | 停用 `albumctl-web.service`，相册统一在 nasctl 网页中使用 |
| 移除远程相册模式 | 配置含 `api_base`、`token`、`public_url` 或 `mode = "remote"` 时 nasctl **拒绝启动** | 删除这些键，改为本地读取索引 |
| `hub.album.worker_binary` | 若仍指向 albumctl，相册任务会报“相册处理程序不可用” | 删除此项（默认使用 nasctl 自身） |
| systemd 单元改名 | `albumctl-thumbs.{service,timer}` → `nasctl-album-thumbs.{service,timer}`，运行用户改为 `nasctl` | 停用旧单元，按需安装新单元 |
| `nasctl-web.service` 新增 `GOMEMLIMIT=96MiB` | Go 堆接近 96 MiB 时提前回收 | 重新安装单元，或在本机 drop-in 中加入同一行 |
| 网页改为“用户页面 + 管理后台” | 地址、入口位置变化 | 无需操作；旧地址自动跳转，见下文 |
| 示例配置改名 | `configs/albumctl.example.toml` → `configs/nasctl.example.toml` | 仅影响参考，实际配置不用改名 |

**不变的部分：** 配置、状态和缓存路径（`/etc/albumctl/config.toml`、`/var/lib/albumctl`、
`/var/cache/albumctl`）、相册索引 schema（仍为 2）、追番数据库、账号、备份计划和原媒体文件
都保持原样，不需要迁移数据。

### 升级前检查

先确认自己属于哪种部署：

```sh
grep -nE 'api_base|public_url|worker_binary|mode *= *"remote"|^\s*token' /etc/albumctl/config.toml
systemctl list-unit-files 'albumctl*' 'nasctl*'
pgrep -a albumctl
```

- **本地相册（默认）：** nasctl 直接读取索引，albumctl 只被它按需启动。升级最简单。
- **远程相册：** 配置里有 `[hub.album] api_base`，另外运行着 `albumctl serve`。如果这个
  albumctl 使用**另一份配置文件或另一个状态目录**，升级时要把它的 `[[collections]]`，以及
  `paths.index_file`、`paths.cache_dir`（或 `paths.state_dir`）并入 nasctl 的配置，让 nasctl
  能找到原来的相册索引和缩略图缓存。
- **使用过旧定时器 `albumctl-thumbs.timer`：** 旧单元以 `albumctl` 用户运行，缓存文件可能
  属于该用户；新单元以 `nasctl` 用户运行，见第 6 步。

### 升级步骤

以下以 arm64、安装目录 `/usr/local/bin` 为例。

**1. 下载并校验新版本**

```sh
sha256sum -c nasctl-linux-arm64.tar.gz.sha256
tar -xzf nasctl-linux-arm64.tar.gz
cd nasctl-linux-arm64
sha256sum -c SHA256SUMS
```

**2. 记录现状**

```sh
mkdir -p ~/nasctl-old-rollback
sudo cp -a /usr/local/bin/nasctl /usr/local/bin/albumctl ~/nasctl-old-rollback/
sudo cp -a /etc/albumctl/config.toml ~/nasctl-old-rollback/
systemctl cat 'albumctl*' 'nasctl*' > ~/nasctl-old-rollback/units.txt 2>&1
```

**3. 停止所有写入者**

```sh
sudo systemctl stop nasctl-web.service
sudo systemctl disable --now albumctl-web.service albumctl-thumbs.timer 2>/dev/null
sudo systemctl stop albumctl-thumbs.service 2>/dev/null
pgrep -a albumctl   # 应当没有输出；有残留任务时等它结束或手动停止
```

**4. 用旧版建立状态检查点**

这一步必须用**旧版** `nasctl`：新版遇到远程相册配置键会拒绝加载配置。

```sh
sudo install -d -m 700 /mnt/backup-private/nasctl-state
sudo /usr/local/bin/nasctl --config /etc/albumctl/config.toml state backup \
  --package /mnt/backup-private/nasctl-state/before-0.3.0 --confirm
sudo /usr/local/bin/nasctl --config /etc/albumctl/config.toml --json state verify \
  --package /mnt/backup-private/nasctl-state/before-0.3.0
```

保存 verify 输出的清单 SHA-256，回滚时要用。目标目录不能位于文件管理器的存储根内。

**5. 修改配置**

编辑 `/etc/albumctl/config.toml` 的 `[hub.album]`：

```toml
[hub.album]
# 删除：mode = "remote"、api_base、token、public_url
# 删除：worker_binary = "/usr/local/bin/albumctl"
mode = "local"        # 可省略；mode = "disabled" 可停用相册
```

远程部署按“升级前检查”并入相册集合和索引路径。然后先把新程序放到旁边，以服务账号
做只读检查：

```sh
sudo install -m 755 ./nasctl /usr/local/bin/nasctl.new
sudo -u nasctl /usr/local/bin/nasctl.new --config /etc/albumctl/config.toml doctor
sudo -u nasctl /usr/local/bin/nasctl.new album --config /etc/albumctl/config.toml doctor
```

`doctor` 报错时先修好配置，不要启动服务。

**6. 安装程序和单元**

```sh
sudo mv /usr/local/bin/nasctl.new /usr/local/bin/nasctl
sudo rm -f /usr/local/bin/albumctl

sudo install -m 644 systemd/nasctl-web.service /etc/systemd/system/nasctl-web.service
sudo rm -f /etc/systemd/system/albumctl-web.service \
           /etc/systemd/system/albumctl-thumbs.service \
           /etc/systemd/system/albumctl-thumbs.timer
sudo systemctl daemon-reload
```

如果原来的 `nasctl-web.service` 有本机修改（挂载依赖、`ReadWritePaths`、资源上限），
改用 drop-in 保留它们：`sudo systemctl edit nasctl-web.service`。不想替换单元文件时，
至少在 drop-in 中加入 `Environment=GOMEMLIMIT=96MiB`。

需要夜间缩略图时安装新定时器。它和以前一样默认不启用，`--collection` 等参数按需修改：

```sh
sudo install -m 644 systemd/nasctl-album-thumbs.service systemd/nasctl-album-thumbs.timer /etc/systemd/system/
sudo systemctl daemon-reload
# 确认后再启用：sudo systemctl enable --now nasctl-album-thumbs.timer
```

使用过旧定时器的部署，检查状态和缓存目录里是否有不属于 `nasctl` 的文件，只修正这两个
目录，**不要改原照片／视频的所有权**：

```sh
sudo find /var/lib/albumctl /var/cache/albumctl ! -user nasctl -print | head
sudo chown -R nasctl:nasctl /var/lib/albumctl /var/cache/albumctl   # 仅在上面有输出时
```

**7. 启动并验证**

```sh
sudo systemctl start nasctl-web.service
systemctl status nasctl-web.service
nasctl --config /etc/albumctl/config.toml status
systemctl show nasctl-web.service -p MemoryCurrent -p MemoryPeak -p Environment
```

在浏览器中确认：

- 能正常登录，首页显示存储空间；
- “相册”能看到原有照片，没有出现“暂时无法读取照片”；
- 管理员或操作员进入“管理后台 → 相册管理”，用很小的上限（如 10 项、2 分钟）跑一次限量
  扫描，在“任务中心”看到任务完成；
- “追番”的订阅、观看记录和“管理后台 → 追番管理”中的下载状态都还在。

**8. 更新外部引用**

- cron、脚本、监控里的 `albumctl …` 改为 `nasctl album …`（参数不变，例如
  `nasctl album thumbs --collection photos --limit 200 --max-runtime 20m`）；
- 反向代理或书签中指向独立相册端口的地址，改为 nasctl 的 `#album`；
- 日志过滤：`journalctl -u nasctl-web -u nasctl-album-thumbs`。

### 网页入口变化

| 以前（桌面应用） | 现在 |
| --- | --- |
| 概览 | 管理后台 → 概览（只读用户在首页查看存储空间） |
| 文件管理、相册、追番 | 顶部导航（手机为底部标签栏）的“文件”“相册”“追番” |
| 追番里的“管理追番” | 管理后台 → 追番管理（订阅增删改、下载控制、连接与调度、元数据、历史） |
| 相册“更多 → 管理相册” | 管理后台 → 相册管理（照片目录、限量扫描／缩略图、暂停与恢复） |
| 备份 | 管理后台 → 备份 |
| 任务中心 | 管理后台 → 任务中心 |
| 设置 | 管理后台 → 系统设置 / 用户与权限 / 操作记录 |
| 头像修改密码、退出登录、主题 | “我的”页面（右上角头像或手机底部“我的”） |
| 桌面自定义应用 | 首页“我的应用” |

旧地址 `#media`、`#overview`、`#jobs`、`#settings`、`#backup` 会自动跳转到对应的新页面。
只读用户看不到管理后台；操作员可进入概览、任务中心、追番管理和相册管理；备份、用户和
系统设置只对管理员开放。桌面搜索框的“搜索应用”已移除，首页保留文件搜索。用户页面的
追番只负责观看，选择／暂停下载等操作在“追番管理”中进行。

升级后首次打开页面会自动加载新界面，不需要清理浏览器缓存。

### 常见问题

- **启动失败：`hub.album: the standalone album service was merged into nasctl …`**
  配置里还有远程相册键，按第 5 步删除。
- **相册管理显示“相册处理程序不可用”**
  `hub.album.worker_binary` 仍指向已删除的 albumctl，删除该项后重启服务。
- **相册显示“你的第一本相册”，看不到原来的照片**
  nasctl 没找到原索引。远程部署要把原 albumctl 的 `paths.index_file`／`state_dir` 和
  `[[collections]]` 并入 nasctl 配置。
- **缩略图任务报权限错误**
  缓存目录里有旧 `albumctl` 用户的文件，按第 6 步修正状态和缓存目录的所有权。
- **`another NAS console already owns this state directory`**
  还有旧服务或进程在运行，确认 `systemctl status` 和 `pgrep -a nasctl` 后再启动。

### 回滚到旧版

0.3.0 没有修改数据库 schema（相册索引仍为 2，追番库仍为 5），也没有新增状态文件，回滚
只需换回旧程序、单元和配置。从 0.1.x 升级时注意：0.2.0 起新增的照片保险箱和 WebDAV 备份
数据会保留，但 0.1.x 不认识它们，回滚到 0.1.x 期间这两个功能不可用。

```sh
sudo systemctl stop nasctl-web.service
sudo cp -a ~/nasctl-old-rollback/nasctl ~/nasctl-old-rollback/albumctl /usr/local/bin/
sudo cp -a ~/nasctl-old-rollback/config.toml /etc/albumctl/config.toml
# 按 ~/nasctl-old-rollback/units.txt 恢复原来的单元文件，然后：
sudo systemctl daemon-reload
sudo systemctl start nasctl-web.service
```

升级后如果状态出现异常，按 [STATE-BACKUPS.md](STATE-BACKUPS.md#恢复) 用第 4 步的检查点和
清单 SHA-256 执行 `state restore`（用能读取当前配置的那个版本执行）。
