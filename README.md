# nasctl —— 给低资源 ARM NAS 的控制台

两个程序，一条原则：**机器不能宕，原文件不能坏，这比功能多少更重要。**

- **`albumctl`** —— 照片和视频索引，加一个只读网页查看器。它会扫描目录、生成
  缩略图，但**永远**是有上限的任务：有明确的数量上限、运行时长上限，前面还有
  一道资源闸门可以随时叫停。
- **`nasctl`** —— 同一台机器上的只读控制台：文件浏览、下载、预览，相册索引，
  看番媒体库，备份状态，以及控制台自己的任务记录。

两个二进制是**故意分开**的。`nasctl` 无法触达解码器、扫描器或任何子进程 ——
这由一条遍历导入图的测试强制保证，不是靠约定 —— 所以对控制台发一个 HTTP 请求，
不可能启动全库扫描。

这里不会移动、改名、删除或改写任何一个原文件。文件写入功能（上传、建目录、
改名、复制、回收站）是**声明了但拒绝执行**的，而且配置校验器会直接拒绝那个
会让它撒谎的开关。

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

相册服务和控制台的只读阶段，开发与离线测试已完成。**尚未在目标机器上运行过
一次。** 这是一条刻意的边界，不是遗漏。因此下面这些事都还没有验证：ARMv7 真机
运行行为、目标机上的内存上限、目标机上的 cgroup 是否真能生效、真实媒体树、真实
的看番数据库 —— 完整清单在
[`docs/HUB-STAGE-B.md`](docs/HUB-STAGE-B.md) §7。

## 编译

只需要 Go 1.27.1 或更新版本。其他什么都不需要：不要 cgo、不要交叉编译器、不要
Docker、不要 Node、不要 Python。前端是直接内嵌进二进制的纯 JavaScript，没有构建
步骤。

```sh
go build ./...                       # 两个二进制
go test ./...                        # 测试套件

# 可重复地构建 ARMv7 产物
SOURCE_DATE_EPOCH=1760000000 ./scripts/build-armv7.sh
sha256sum -c dist/SHA256SUMS
```

产物是静态链接的 32 位 ARM 二进制，不依赖任何共享库，所以目标机上不需要预装
任何东西。同一个 commit、同一个 `SOURCE_DATE_EPOCH` 构建两次，字节完全一致 ——
这正是校验值有意义的前提，CI 每次推送都会检查这一点。

## 运行

`albumctl` 和 `nasctl` 读同一个配置文件。复制样例再改路径：

```sh
cp configs/albumctl.example.toml /etc/albumctl/config.toml
$EDITOR /etc/albumctl/config.toml

albumctl doctor --config /etc/albumctl/config.toml    # 只读体检
nasctl   doctor --config /etc/albumctl/config.toml
```

样例里**每一个策略值都是内置默认值**，所以删掉某一行就等于恢复默认。只有
`[paths]` 和 `[[collections]]` 是本机相关、必须改的。键名拼错会**按名字报错拒绝**，
而不是被忽略 —— 这个文件里大多数数字都是上限，悄悄丢掉一个，你就会以为某道限制
在生效，其实没有。

然后，一步一步、手动地来：

```sh
# 控制台，只读。它自己不会启动任何工作。
nasctl serve --config /etc/albumctl/config.toml

# 相册任务，有上限，一次一个 collection。
albumctl scan   --collection photos --limit 500 --dry-run   # 先预览
albumctl scan   --collection photos --limit 500
albumctl thumbs --collection photos --limit 200 --max-runtime 20m
```

**默认不做任何定时调度。** 夜间定时器是随包提供的，但处于禁用状态，启用它是
一个需要单独决定、单独授权的步骤。

`albumctl thumbs` 的退出码是给调度器读的：**0** 完成了这次有上限的工作（包括
"队列本来就是空的"）；**3** 因策略原因提前停下，需要人来判断；**4** 因资源阈值
停下，**不允许循环重试**。有上限的任务返回非零码，含义永远是"没有再往下做"，
绝不是"机器被逼得更狠了"。

## 访问控制

控制台**没有逐用户认证**。它的访问控制就是监听地址，而且这件事是在启动时
**被检查**的，不是写在文档里让你自己注意：绑到公网地址会被拒绝启动，除非操作者
为离线测试显式放行。请绑到 Tailscale 地址，或者绑回环地址再用 SSH 隧道访问。

可选的共享 token 会增加一道 bearer 校验；它区分的是"有 token"和"没 token"，
所有持有者都是同一个身份。

AI 调用者可以分配**各自的凭据**（`[[hub.agents]]`）。身份由凭据决定，所以 agent
无法靠"不声明自己是谁"来绕过限制：它的读取会被记进审计日志，除非按 agent 单独
放行否则不能启动重任务，而且它不能给自己批准。

## 明确不做的事

不做 AI 分类、人脸识别、OCR。不做视频实时转码。不做全库持续监听，不做后台无限
循环。不移动、不改名、不删除原文件，不写回 EXIF。不做网盘上传、同步或备份逻辑 ——
那些继续交给现有工具，本项目是**让路**给它，而不是替代它。不提供任意 shell，
也没有任何接受服务器路径的操作。

## 文档

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
