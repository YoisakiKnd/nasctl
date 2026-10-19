# 存储与共享

“设置 › 存储与共享”管理 NAS 上的磁盘、USB 设备、Samba 局域网共享和网络存储（SMB / NFS），
功能参考 CasaOS 的存储页面。它是**可选的**：不启用存储助手时，nasctl 的其他功能照常工作，
这个页面只显示启用方法。

## 为什么需要存储助手

nasctl 控制台以普通用户 `nasctl` 运行，并在 systemd 沙箱里（`ProtectSystem=strict`、
`PrivateDevices=yes`、`NoNewPrivileges=yes`、没有任何 capability）。挂载、格式化、修改
`/etc/fstab` 和 Samba 配置都需要 root，控制台自己做不到，也不应该做到。

所以 root 权限交给一个**按需启动的存储助手**：

- `nasctl-storage.socket` 在 `/run/nasctl/storage.sock` 监听，权限 `0660 root:nasctl`，
  只有 nasctl 服务用户能连接。
- 每个连接启动一个 `nasctl-storage@.service` 实例（`nasctl storage-helper`，root），
  读一个 JSON 请求、执行、回答、退出。**没有常驻的 root 进程**，空闲时不占内存。
- 助手不信任控制台发来的任何内容，自己再检查一遍：
  - 设备必须出现在当前的磁盘列表里；**系统盘**（挂载了 `/`、`/boot`、`/usr`、`/var`、
    `/home` 等或交换分区的磁盘）一律拒绝挂载、卸载和格式化。
  - 只挂载到 `/media/nasctl/名称`，名称只能是字母、数字、`.`、`_`、`-`，最多 32 个字符；
    同名目录里已有文件时拒绝。
  - 只挂载 ext2/3/4、XFS、Btrfs、F2FS、FAT32、exFAT、NTFS；一律带 `nosuid,nodev`。
    FAT32、exFAT、NTFS 挂载时文件属于 nasctl 用户。NTFS 优先用内核的 ntfs3 驱动；
    内核没有 ntfs3 时才用 ntfs-3g，并通过 `systemd-mount` 挂载，让它的后台进程不随
    助手退出。
  - 存放正在使用的交换文件的分区也算系统盘。
  - 格式化只针对整块非系统盘，必须先卸载所有分区，并输入磁盘**序列号**（没有序列号时
    输入设备名）确认。结果是一个 GPT 分区、ext4 文件系统，自动挂载并设为开机挂载。
  - 共享文件夹必须在已配置的存储位置或 `/media/nasctl` 里（符号链接解析后再判断），
    `/etc`、`/usr`、`/var/lib`、`/root` 等系统目录即使被写进存储位置也拒绝。
  - 共享名、服务器地址、用户名等都按严格的格式检查后才写进命令行或配置文件。
- 每次修改都会写入“操作记录”（`storage.mount`、`storage.format` 等）；密码不会出现在
  记录里。

助手必须运行在主机的挂载命名空间里，所以它的单元**不能**加 `PrivateTmp`、`ProtectSystem`、
`ProtectHome` 等会建立独立命名空间的选项，否则它挂载的磁盘对其他程序不可见。

## 启用

发布包里带有需要的文件。以 root 执行：

```sh
install -m 0644 systemd/nasctl-storage.socket systemd/nasctl-storage@.service \
  systemd/nasctl-storage-automount@.service /etc/systemd/system/
install -m 0644 udev/99-nasctl-automount.rules /etc/udev/rules.d/
install -m 0644 tmpfiles/nasctl.conf /etc/tmpfiles.d/
systemd-tmpfiles --create /etc/tmpfiles.d/nasctl.conf
# 0.8.0 的 nasctl-web.service 增加了 ReadWritePaths=-/media/nasctl
install -m 0644 systemd/nasctl-web.service /etc/systemd/system/
systemctl daemon-reload && udevadm control --reload
systemctl enable --now nasctl-storage.socket
systemctl restart nasctl-web
```

如果你改过 `nasctl-web.service`，不要覆盖，只加一行 `ReadWritePaths=-/media/nasctl`
（或放进 drop-in）。

按需安装的软件（Debian / Ubuntu / Armbian 包名）：

| 软件包 | 用途 |
| --- | --- |
| `smartmontools` | 硬盘健康（SMART）。smartctl 不认识的 USB 硬盘盒会再用 SAT 方式试一次；仍不转发 SMART 的会显示“不支持健康检查” |
| `samba` | 局域网共享 |
| `cifs-utils` | 连接 SMB 网络存储 |
| `nfs-common` | 连接 NFS 网络存储 |
| `exfatprogs`、`ntfs-3g` | 挂载 exFAT / NTFS（新内核自带 exFAT 和 ntfs3 驱动时可不装） |

停用：`systemctl disable --now nasctl-storage.socket`，删除上面安装的文件即可。已经写进
`/etc/fstab` 的开机挂载不会被删除，可以在页面里先“卸载”并勾选“同时取消开机自动挂载”。

## 在“文件”里浏览挂载的磁盘

磁盘挂载在 `/media/nasctl/名称`。在“系统设置 › 存储位置”里添加一个目录 `/media/nasctl`
（需要整理文件时打开“允许文件整理”），重启 nasctl 一次。之后新挂载的磁盘、USB 设备和网络
存储都会作为子文件夹直接出现，不用再改配置。

挂载是在控制台启动之后由助手完成的。systemd 会把新挂载传播进控制台的沙箱，所以不需要
重启控制台。

## 开机挂载与 /etc/fstab

勾选“开机自动挂载”时，助手在 `/etc/fstab` 末尾维护一个区块：

```
# BEGIN nasctl managed mounts - edited by nasctl, keep these markers
UUID=… /media/nasctl/backup ext4 noatime,nosuid,nodev,nofail,x-systemd.device-timeout=10s 0 2
# END nasctl managed mounts
```

- 按 UUID 挂载，带 `nofail` 和 10 秒设备等待：磁盘不在时**不会卡住开机**。
- 同一个分区换名称再勾选开机挂载时，旧名称的行会被替换，不会开机挂两次。
- 区块以外的行原样保留，文件权限不变。第一次修改前，原文件备份为 `/etc/fstab.nasctl-backup`。
- 网络存储的行带 `_netdev,nofail` 和 30 秒挂载超时。

## USB 自动挂载

页面上的“插入 USB 硬盘时自动挂载”默认关闭。打开后，udev 规则在 USB 设备上出现文件系统时
启动 `nasctl-storage-automount@设备.service`，助手用 `systemd-mount` 把它挂到
`/media/nasctl/卷标`（重名时加 `-2`、`-3`）。拔出设备时 systemd 自动卸载。

`/etc/fstab` 里（nasctl 区块以外）已经按 UUID、PARTUUID、卷标或设备名登记的分区不会被
自动挂载：开机时带 `nofail` 的挂载不会等 USB 硬盘，数据盘可能先于它出现，重新插拔时也一样，
它应当留给 fstab 挂到原来的位置。

自动挂载不会写 `/etc/fstab`；想要开机挂载的 USB 硬盘，在页面上卸载后再手动挂载并勾选
“开机自动挂载”。拔出前最好先在页面上点“卸载”，避免正在写入的文件损坏。

## 局域网共享（Samba）

- 共享配置写在 `/etc/samba/nasctl-shares.conf`，助手只在 `/etc/samba/smb.conf` 末尾加一行
  `include = /etc/samba/nasctl-shares.conf`，不改动其他内容。
- 共享名不能和 smb.conf 里已有的共享同名（不分大小写）：Samba 会把同名段落合并，等于改写
  原来的共享。
- 每次保存都先用 `testparm` 校验，不通过就恢复原来的共享；通过后让 smbd 重新加载配置，
  已连接的电脑不会断开。
- 需要密码的共享用账号 `nasctl-share` 登录。第一次设置共享密码时助手会创建这个系统用户
  （不能登录 shell）并用 `smbpasswd` 设置密码。
- 所有共享都以 `force user = nasctl` 读写，文件归 nasctl 服务用户所有，和网页里的权限
  一致，不会出现网页里删不掉的文件。
- “无需密码访问”依赖 Samba 的 `map to guest = bad user`。Debian / Ubuntu / Armbian 的
  默认 smb.conf 已经有这一行；其他发行版没有时请自己加到 `[global]`。

访问方法：Windows 在资源管理器地址栏输入 `\\NAS地址`，macOS 在访达按 ⌘K 输入
`smb://NAS地址`，Linux 文件管理器输入 `smb://NAS地址`。

## 网络存储（SMB / NFS）

把另一台 NAS 或电脑上的共享挂到 `/media/nasctl/名称`，就能在文件页浏览，也能作为备份
目标。

- SMB 的账号密码保存在 `/etc/nasctl/credentials/名称`（`0600 root`），通过 `credentials=`
  传给 mount.cifs，不出现在进程列表、fstab 或网页里。
- 文件归 nasctl 服务用户（`uid=`/`gid=`），NFS 使用 `soft,timeo=150,retrans=3`，远端离线时
  不会让程序永久卡住。
- 添加时最多等 30 秒：服务器不可达或拒绝连接会在这之内报错（NFS 这次挂载不重试，开机挂载
  照常重试）。
- “断开并移除”会卸载、删除凭据和开机挂载行。

## 文件位置

| 路径 | 内容 |
| --- | --- |
| `/media/nasctl/` | 所有挂载点 |
| `/etc/nasctl/storage.json` | 自动挂载开关、共享和网络存储列表（不含密码） |
| `/etc/nasctl/credentials/` | SMB 网络存储的凭据 |
| `/etc/fstab` | 开机挂载（nasctl 区块） |
| `/etc/samba/nasctl-shares.conf` | Samba 共享 |

## 排查

- 页面一直显示“启用存储助手”：`systemctl status nasctl-storage.socket`，确认
  `/run/nasctl/storage.sock` 存在且属于 `nasctl` 组。
- 操作失败的详细原因在错误提示的“详情”里，也可以看
  `journalctl -t nasctl-storage`。
- 挂载成功但文件页看不到：确认已把 `/media/nasctl` 加为存储位置并重启过 nasctl，且
  `nasctl-web.service` 里有 `ReadWritePaths=-/media/nasctl`（需要写入时）。
- 卸载失败“还有程序在使用”：停止正在复制、播放或备份的任务后重试；
  `fuser -vm /media/nasctl/名称` 可以看到是谁在用。

## 验证范围

助手的每项检查都有单元测试（临时目录里的 fstab、Samba 配置和模拟命令）。在真实 Linux
（systemd 255）上验证过：socket 按连接启动 root 助手；对一块 512 MB 的网络块设备分区、
格式化、挂载、写入并恢复 fstab；在与 nasctl-web 相同沙箱设置的服务里能连上助手、能看到
并写入它启动后才挂载的磁盘。Samba 在 Ubuntu 24.04 容器里用 smbclient 验证过密码共享读写、
错误密码被拒、访客只读。USB 自动挂载、SMART、真实 SMB/NFS 服务器需要在目标设备上确认。
