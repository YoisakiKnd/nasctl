# 账号、首次设置与本机恢复

nasctl 提供本机账号：管理员、操作者、只读用户，最多 32 个。所有成员共享配置的文件根
与媒体库，当前不提供成员独立文件夹 ACL。服务端检查角色和任务归属；隐藏后台入口不是
权限边界。

| 角色 | 操作范围 |
| --- | --- |
| 管理员 | 全部文件／媒体、备份、设备配置、成员与授权、全局模式和审计 |
| 操作者 | 整理可写根中的文件，管理追番／相册任务，查看与取消自己创建的任务 |
| 只读用户 | 浏览文件、照片与媒体库，播放媒体；不能修改文件或查看任务与管理配置 |

成员可在“我的”页面修改自己的密码，需要验证当前密码。角色、密码、停用状态或
删除账号会撤销该账号原有会话。至少保留一个已启用管理员。用户名不可改名；重新创建
同名账号得到新的 ID，不能继承旧账号任务。

观看状态目前由共享媒体库记录，不是成员独立进度。只读用户读取脱敏的订阅与播放
信息，私有 RSS／下载链接、下载器配置和历史管理导出不属于只读浏览权限。

## 新设备首次设置

在 NAS 本机使用独立配置与状态路径：

```sh
nasctl --config /etc/albumctl/config.toml init \
  --state-dir /var/lib/albumctl --bind 127.0.0.1:8090 --confirm
nasctl --config /etc/albumctl/config.toml serve
```

`init` 不覆盖已有配置，生成 0600 的 TOML 和私有首次设置密钥，启用 `[hub] setup=true`。
可以暂不配置存储目录。网页先显示首次设置，文件和媒体 API 仍要求认证。在另一终端读取：

```sh
nasctl --config /etc/albumctl/config.toml users setup-code
```

由设备操作者将密钥填入自己的首次设置页，创建用户名与至少 12 个字符的密码。密钥
不可分享，只能用于第一个管理员；成功后旧密钥不能重放。随后在“设置 → 设备配置”
添加 NAS 上已经存在的存储路径，校验、确认保存并重启生效。

基础 TOML 必须保留 `hub.setup=true` 或 `server.token`，让状态盘完全丢失时仍要求
认证。已有无令牌配置兼容匿名本地操作者，但创建第一个账号前必须先启用上述基础保护
并重启。已有账号而基础保护缺失时拒绝启动，不会退回匿名访问。已使用的设备应按下方
恢复流程处理损坏或丢失的账号，不能通过重新初始化绕过保护。

## 会话与密码存储

- 会话是独立的 256 位随机 Cookie，HttpOnly、SameSite=Strict、Path=/。直接 HTTPS 请求
  另设 Secure，不信任客户端代理头。密码／令牌不进入 URL 或 localStorage。
- 绝对有效期 12 小时。退出、服务重启、账号版本变化均撤销旧会话。最多保留 64 个会话，
  超出时淘汰最早会话；API 返回 401 时，前端卸载全部受保护页面并要求重新登录。
- 同一来源地址一分钟内失败 10 次后限制登录／密码验证，来源表最多 256 项。不使用
  X-Forwarded-For，同一反向代理后的客户端共享来源限额。
- `state_dir/nasctl-users.json` 最多 64 KiB，权限 0600，密码使用独立随机盐和
  PBKDF2-HMAC-SHA256 600000 次派生，对应 [OWASP 的 PBKDF2 参数建议](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)。
  每个存储实例同时最多一个派生，忙时返回 429。未知或停用账号也执行固定成本验证。
  此参数说明不代表整个部署经过认证，真机性能仍需测量。
- 密码／角色保存经过确认、任务和审计，秘密不进入任务参数或 API 响应。纯账号／设备
  配置维护可在全局暂停时执行，仍受角色、只读控制台与互斥限制。
- 账号文件丢失、损坏或权限／类型不安全时拒绝认证及启动，不自动创建空账号。

登录表单不提供传输加密。继续使用允许的私网／回环监听地址，远程访问通过 HTTPS、
Tailscale 或 SSH 隧道。反向代理终止 HTTPS 时需保护代理到 NAS 的链路；当前不通过
任意 forwarded header 改变 Cookie 策略。

## 忘记密码

OS 操作者可重置一个已有账号并保留其他成员。密码只通过标准输入读取，不能放在命令
参数、环境变量或脚本仓库。以下示例在 Bash 中隐藏输入：

```bash
read -r -s -p 'New NAS password: ' nasctl_password
printf '\n'
printf '%s\n' "$nasctl_password" | nasctl --config /etc/albumctl/config.toml \
  users password --username owner --password-stdin --confirm
unset nasctl_password
```

本机还提供 `users list|create|set|remove`，用 `-h` 查看参数。第一个账号必须明确
`--role admin`，后续默认只读。删除要求准确的 `--confirm-username`。写命令要求
`--confirm`，拒绝 AI 身份；OS 文件访问权限本身属于设备管理权限。

## 账号文件损坏或丢失

先停止 nasctl、`nasctl album` 任务和其他状态写入进程。普通忘记密码不要使用此命令：它用一个
新管理员替换全部账号，撤销所有旧成员会话；原件保存在私有恢复目录。既有文件、追番
记录、任务库与基础配置不被此命令修改。

```bash
read -r -s -p 'Recovered administrator password: ' nasctl_password
printf '\n'
printf '%s\n' "$nasctl_password" | nasctl --config /etc/albumctl/config.toml \
  users recover --username owner --password-stdin --confirm
unset nasctl_password
```

此路径只加载基础 TOML，损坏的设置覆盖文件或任务数据库不会阻止账号修复。状态独占锁
拒绝运行中的服务；未处理的状态恢复日志须先执行 `state recover`。先写持久账号保护标记，
再保留原件并发布新账号。中途退出仍拒绝匿名访问，停止全部进程后可以重试；请保留
输出的私有目录供检查。

记录位于 `state_dir/local-recovery/<随机编号>`，含意图、结果和存在时的 `original.raw`。
不复制或解析损坏原件，不跟随符号链接。最多保留 16 次，满后拒绝修复；先人工检查并
移至私有离线存储，程序不自动删除。原件可能含密码派生值或下载器秘密，恢复目录必须
私有。设置重置共用此目录与上限，见 SETTINGS.md。

账号文件及启用标记参与服务状态包。已有账号时，不能恢复不含账号的旧包以清掉登录
保护。恢复账号备份也恢复当时密码与角色，重启前核对，详见 STATE-BACKUPS.md。

## 令牌与 API 兼容

`server.token` 继续作为设备管理员 Bearer 凭据，也可通过网页换取会话；与成员并存时
仍有管理员权限。AI 凭据独立配置于 `hub.agents`，不能创建管理员／成员会话或管理账号
与设备。显式无效 Authorization 不回退到 Cookie。

| 路由 | 请求／行为 |
| --- | --- |
| `GET /api/v1/auth/status` | 最小登录／设置状态；已认证时附当前角色和成员名称 |
| `POST /api/v1/auth/setup` | `code,username,display_name,password`，私有密钥创建首个管理员 |
| `POST /api/v1/auth/login` | `username,password` 或单独 `token`；签发 Cookie |
| `POST /api/v1/auth/logout` | 撤销当前会话并清除 Cookie |
| `POST /api/v1/auth/password` | `current_password,password`；只修改当前成员，无目标账号字段 |
| `GET /api/v1/users` | 管理员读取脱敏成员、版本和设置状态 |
| `POST /api/v1/users` | 管理员提交 `revision,id?,username,display_name,role,disabled,password?` |
| `DELETE /api/v1/users/{id}` | 管理员提交 `revision,confirm_username`，准确确认删除 |

setup、password 和成员写入均要求 `X-Nasctl-Confirm: 1`。账号请求拒绝未知字段、超过
8 KiB、多份 JSON 和旧 revision。shell 与登录／首次设置端点公开加载，私有数据仍受
认证保护；写接口继续拒绝跨站请求，账号响应禁止缓存。
