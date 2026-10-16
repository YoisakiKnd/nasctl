# 升级与迁移

本文按版本记录需要手工处理的变化。通用的检查点、升级和回滚原则见
[DEPLOYMENT.md](DEPLOYMENT.md#升级与检查点)，状态包细节见 [STATE-BACKUPS.md](STATE-BACKUPS.md)。

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
