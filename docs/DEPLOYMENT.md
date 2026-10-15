# 部署、升级与兼容版本回滚

nasctl 面向 Linux NAS，提供 amd64、arm64、ARMv7 构建。只启动一个 nasctl 服务；
albumctl 是按需启动的扫描／缩略图处理组件，不需要独立相册 HTTP 服务。内存按任务
数量和固定流缓冲控制，文件大小不决定分配同等大小的内存。

| Linux 用户空间 | Go 构建目标 |
| --- | --- |
| x86_64／64 位 | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` |
| aarch64／64 位 | `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` |
| ARMv7／32 位 | `GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0` |

使用 go.mod 要求的 Go 1.27.1 或更新工具链，CI 固定 1.27.1。nasctl 与 albumctl 使用
同一份源码／编译器构建，安装在同一目录。前端已嵌入；NAS 不需要 Node、Svelte 或
Python。修改 UI 后，在开发机先运行 `npm ci --prefix frontend`、`npm run check --prefix frontend`
和 `npm run build --prefix frontend`，再编译 Go。ARMv7 打包入口为
`scripts/build-armv7.sh`，相同工具链与 SOURCE_DATE_EPOCH 可复验构建摘要。

GitHub Actions 的 `ci` 在测试、竞态、ARMv7 冒烟与前端验收全部通过后，上传
`nasctl-linux-amd64`、`nasctl-linux-arm64`、`nasctl-linux-armv7` 三份安装包，保留 30 天。
它们是开发构建，BUILD.txt 记录完整提交、工具链与时间，不会自动发布版本或安装到 NAS。
在 Actions 对应运行的 Artifacts 区下载适合架构的包；也可用已登录的 GitHub CLI：

```sh
gh run download RUN_ID --repo YoisakiKnd/nasctl --name nasctl-linux-arm64 --dir ./nasctl-download
cd nasctl-download
sha256sum -c nasctl-linux-arm64.tar.gz.sha256
tar -xzf nasctl-linux-arm64.tar.gz
cd nasctl-linux-arm64
sha256sum -c SHA256SUMS
```

包内含匹配的两个程序、配置样例、systemd 单元和部署说明。开发机 Linux 可通过
`bash scripts/package-linux.sh amd64|arm64|armv7` 生成相同结构；此打包脚本需要 GNU tar、
readelf、file 与 sha256sum，目标 NAS 日常运行不需要这些构建工具。

正式安装包发布在 [GitHub Releases](https://github.com/YoisakiKnd/nasctl/releases)，不受
Actions 产物 30 天保留期限制。`vX.Y.Z` 标签须与 VERSION 一致，且对应提交已通过 main
上的完整 CI。Release 工作流将版本标识设为 `X.Y.Z`（无 `-dev`），构建三架构，在草稿中
上传并下载核对字节，全部成功后才公开。手动运行默认只验收产物，不公开发版。
若发布流程需要修复，可在 main 更新工作流，然后手动指定已有 `release_tag` 重试；
构建始终检出该标签的固定提交并检查其 main CI，不移动已有版本标签。正式发布时将
`dry_run` 设为 false，已公开的 Release 拒绝覆盖。

## 全新安装

以下命令只适用于全新目录。已有 albumctl 的状态应保持原所有权；让 NAS 单元使用
同一运行用户，先建立私有状态检查点。不要为了安装而递归改变原照片／视频的所有权。

```sh
sudo useradd --system --home /var/lib/albumctl --no-create-home --shell /usr/sbin/nologin nasctl
sudo install -d -o nasctl -g nasctl -m 750 /etc/albumctl /var/lib/albumctl /var/cache/albumctl
sudo install -m 755 /path/to/built/nasctl /usr/local/bin/nasctl
sudo install -m 755 /path/to/built/albumctl /usr/local/bin/albumctl
sudo -u nasctl /usr/local/bin/nasctl --config /etc/albumctl/config.toml init \
  --state-dir /var/lib/albumctl --cache-dir /var/cache/albumctl \
  --bind 127.0.0.1:8090 --confirm
sudo install -m 644 systemd/nasctl-web.service /etc/systemd/system/nasctl-web.service
sudo systemctl daemon-reload
sudo systemctl start nasctl-web.service
sudo -u nasctl /usr/local/bin/nasctl --config /etc/albumctl/config.toml users setup-code
```

使用自己的浏览器完成首次管理员设置。远程可通过 SSH 将 NAS 的回环端口转发到本机，
或部署 HTTPS／Tailscale；地址检查继续拒绝未经明确配置的公网监听。首次密钥、账号、
角色和本机恢复见 AUTH.md。需要开机启动时，再运行 `sudo systemctl enable nasctl-web.service`。
定时追番、自动下载及独立相册 timer 均不会因安装而开启。

在“设置 → 设备配置”添加 NAS 上已准备好的存储根，默认只读。开启文件整理、下载或
媒体归档时，同时给服务用户相应文件系统权限，并为实际目录添加 systemd drop-in：

```ini
[Unit]
RequiresMountsFor=/mnt/data

[Service]
ReadWritePaths=/mnt/data/files /mnt/data/downloads/anime /mnt/data/library/anime
```

路径应对应已经配置的真实挂载；先核对 `findmnt`／fstab。应用不会挂载设备，也不会在
缺失卷上创建空媒体根。应用中的可写确认与 systemd 文件系统权限需同时满足。修改
统一设置、监听或 drop-in 后，等当前任务停止再重启；无需重启第二个相册 HTTP 服务。
同一状态目录的第二个 nasctl 会在打开数据库前拒绝，普通本机 CLI 仍可使用。

## 相册处理的隔离与资源

Linux 缩略图要求 cgroup v2 的 memory controller 和本服务的 `Delegate=yes`。随附
单元开放本服务的 cgroup 子树，NAS 与处理组件用私有启动门在独立子组中工作。默认
解码器上限为 128 MiB，外层单次处理组增加 64 MiB 的监督预算；整个单元暂定
MemoryHigh=192M、MemoryMax=256M，包含 NAS 与子进程。**这不是实际 RAM 用量测量。**
若本机配置提高了解码预算，需核对单元总上限与设备余量；界面不会替你关闭强制隔离。

只读 `doctor` 不建立新 cgroup，最终隔离须用少量受限任务的资源报告确认。浏览页面
只读索引与缓存，缺失缩略图不触发解码。Linux 缺少委派时任务明确拒绝；macOS 开发
预览不伪造 /proc 或 cgroup 数据。部署后记录：

```sh
systemctl show nasctl-web.service -p MemoryCurrent -p MemoryPeak -p MemoryMax -p Delegate
journalctl -u nasctl-web.service -n 50
```

字段或硬件信号缺失记为未知。先用少量照片测量，再按实际设备选择预算，不能把交叉
编译、QEMU 或合成图片当成真实 NAS 内存／吞吐量结果。任务达到资源阈值会保留停止
原因，不自动高频重启；单元保留 `Restart=no`。

文件上传／复制默认 8 GiB，本机可配置至 1 TiB；上传 30 分钟、复制／备份 2 小时等
独立时限仍有效，慢速连接应选择可完成范围。64 KiB 流缓冲、条目与深度限制不随字节
预算升高而取消。大预算的配置／保存／恢复已测试，不代表写入过 1 TiB 实数据。

## 升级与检查点

1. 记录当前两个二进制的 SHA-256、源码版本、工具链、配置与单元／drop-in。准备新
   二进制，先验证构建和摘要，不直接覆盖运行中的文件。
2. 停止 nasctl、独立 albumctl 服务／任务及所有状态写入 CLI。不要删除锁文件来绕过
   “仍在使用”的检查；旧版程序不参与新锁协议，必须主动停止。
3. 用 `nasctl state backup --package 新私有目录 --confirm` 建立检查点，随后独立
   `state verify`，保存清单摘要。目录不在文件管理器根内，父目录 0700。旧版若无该
   命令，可用准备好的新版本离线 CLI 创建并验证；失败时保留原状态，先解决兼容问题。
4. 保留旧二进制，将新的两个二进制以新文件写入同一安装目录、校验后再替换正式名称。
   配置、账号、原媒体和缓存不要当成程序目录一起替换。
5. 启动一个 NAS 服务，核对配置／待重启标记、账号登录、原订阅和媒体数量、原文件样本
   摘要、文件只读／可写边界及少量实际任务。升级不会自动开启 RSS 调度。

状态包使用 SQLite 备份 API 包含提交 WAL，暂存为独立数据库；不要用运行中的单个
`.sqlite` 文件副本冒充完整检查点。包含秘密，请留在私有本地存储。完整步骤、恢复日志
和追番编号保护见 STATE-BACKUPS.md。

## 回滚边界与步骤

回滚要求候选版本能理解当前配置、账号格式和状态包；不能将任意旧二进制套到更新的
数据库上。先在私有检查点上运行候选版本的 `state verify`。新账号格式、提高后的预算
或未来 schema 被候选版本拒绝时，停止回滚，保留原件与检查点。状态恢复不是通用
数据库降级工具；尤其不能丢掉已经提交给外部下载器的编号高水位。

对验证兼容的版本：先停止全部状态写入者，用仍能读取当前状态的新版本 CLI 再建立
“回滚前”检查点。按已核对的清单摘要执行 `state restore`；它还会建立自动检查点并
保留追番编号高水位。基础 TOML 不自动激活，需人工审阅备份配置和当前登录保护。
随后安装匹配的旧二进制对并启动，重复账号、原记录、文件摘要和读写边界验证。
若回滚失败，保持停止状态，用回滚前检查点和新二进制恢复，不重复启动碰运气。

中途退出且存在恢复日志时，先运行 `state recover --confirm`。普通忘记密码用
`users password`；损坏账号用明确替换全体账号的 `users recover`；损坏覆盖配置用
`settings reset`。它们都保留私有恢复原件，不能删除标记或创建空库替代恢复。

## 验收与真机待验

`docs/COMPLETION-PLAN.md` 记录实际完成范围。`scripts/verify-native-album-linux.sh --fixture-only ABSOLUTE_BINARY_DIRECTORY`
在隔离 Linux VM／CI 上用唯一 /run 单元、真实二进制和生成照片验收默认强制隔离、两次
任务及原件摘要，结束停止单元。它需要非 root 调用者、sudo、Go、Python 和 systemd，
这些是开发机要求，不是 NAS 日常运行依赖。

真实 NAS 仍须核对挂载断开／重新连接、断电恢复、磁盘满／配额、网络文件系统语义、
目标架构的内存与压力、设备编解码样本及实际 qBittorrent／TMDB 账号。没有环境时保留
这些待验项；本机合成与故障测试不冒充真机结果。
