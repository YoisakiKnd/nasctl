# 玩客云看番方案

调研日期：2026-10-16。按常见 S805／ARMv7／1 GB 玩客云、已刷可运行 nasctl 的 Linux 分析；未登录或测试用户的真实设备，实际内存、系统、挂载和播放器能力仍需检查。

## 结论

电视和手机原生播放器适合“玩客云下载、存储、原样传输，客户端解码”。首先从单用户直播放开始验证，不承诺多路并发或实时转码能力。

| 方案 | 可行性与建议 |
| --- | --- |
| Emby Server ARMv7 | 官方 Linux 下载页仍列出 Debian/Ubuntu ARMv7（armhf）。系统和运行库兼容时可试，建议原生安装，使用直播放并关闭服务端转码。不能仅凭安装包架构保证特定固件能正常运行。 |
| 现行 Jellyfin | 自 10.11 起已移除 ARM32 支持；32 位玩客云不能通过换 64 位系统解决 CPU 架构限制。建议将 Jellyfin 放在另一台受支持的电脑，读取玩客云上的媒体目录。 |
| 旧 Jellyfin 10.10.x | 曾提供 armhf，但不建议为此长期固定旧版本；不作为本项目的默认部署路线。 |
| 文件共享 + Kodi 等播放器 | 更轻的替代方案。通过已配置的 SMB/NFS 读取媒体，由电视/手机解码；Kodi 也能自行建立媒体库。nasctl 当前不会自动安装或配置 SMB/NFS 服务。 |
| nasctl 网页播放 | 现有播放器直接传输媒体、不进行视频转码；能否播放取决于浏览器支持的编码和封装，不能替代兼容性更强的原生播放器。 |

这是依据架构支持和任务负载作出的建议，并非玩客云上的实测性能排名。不要仅因 S805 有视频解码能力，就认定 Linux 下的 Emby 能使用它做硬件转码；官方支持列表不构成这种保证。

## Emby 试运行配置

1. 确认 `uname -m`、发行版、`free -m` 和媒体盘可用空间；安装匹配系统的官方 armhf 包。此次开发未在真实玩客云安装 Emby。
2. Emby 的资料库指向 nasctl 归档后的媒体目录，建议只读挂载/权限；元数据、缓存和日志使用独立目录。下载临时目录不作为正式媒体库。
3. 关闭对应用户的服务端视频/音频转码能力、字幕烧录及预览图/章节图/片头分析等后台处理。首次扫库和 nasctl 下载/压缩备份错开进行；不要把硬件加速选项作为解决性能问题的前提。
4. 原生客户端选择原画/原始码率，确保局域网带宽足够。使用能原生解码目标 H.264/HEVC、音轨和字幕的播放器；ASS/PGS 等字幕在部分客户端会触发烧录，优先由客户端渲染字幕。
5. 播放一集典型文件，在 Emby 会话状态中确认 Direct Play，观察 CPU、内存、交换和磁盘读速；若发生转码，先调整客户端/素材格式，不以降低清晰度来要求玩客云实时转换。
6. 确认实际手机/电视客户端的播放许可：Emby 部分客户端可单独解锁基础播放，Premiere 覆盖更多功能。先验证兼容与直播放，再决定是否付费。

nasctl 主页可添加 `http://{host}:8096/` 快捷方式。它只打开 Emby 页面，不会安装、托管或反向代理 Emby。Jellyfin 与 Emby 默认端口都常用 8096，若同时部署需分别配置端口。

## 官方资料

- [Jellyfin 10.11 ARM32 移除公告](https://jellyfin.org/posts/jellyfin-release-10.11.0/#removal-of-arm32-armhf-support)
- [Jellyfin 硬件选择](https://jellyfin.org/docs/general/administration/hardware-selection/)
- [Emby Linux 安装与架构](https://emby.media/linux-server.html)
- [Emby 直播放与转码触发条件](https://emby.media/support/articles/Transcoding.html)
- [Emby Linux 硬件加速范围](https://emby.media/support/articles/Hardware-Acceleration-on-Linux.html)
- [Emby Premiere 与客户端解锁](https://emby.media/premiere.html)
- [Kodi 添加网络媒体源](https://kodi.wiki/view/Adding_video_sources)
