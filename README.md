# wechat-profile-bot

微信 ClawBot（iLink Bot API）服务端 —— 把微信聊天记录自动存到数据库，生成人物画像和意图分析。

配套 Windows 桌面端：[github.com/caodabao99/wechat-profile](https://github.com/caodabao99/wechat-profile)

## 功能

- **微信消息收发**：通过腾讯官方 iLink Bot API 长轮询收发消息，无需公网 IP/Webhook/内网穿透
- **聊天记录识别**：直接粘贴微信多选复制的聊天记录，自动解析并存入 SQLite
- **人物画像**：AI 自动生成/更新联系人画像（性格、兴趣、沟通风格、关系等）
- **意图分析**：分析对方最新消息的潜在意图、情绪、建议回复
- **联系人管理**：备注、合并（换昵称后关联）、撤销合并、删除
- **REST API**：内置 HTTP 接口（默认端口 17965），Windows 桌面版可远程复用同一份数据与模型分析

## 快速开始

### 1. 获取程序

从 [Releases](https://github.com/caodabao99/wechat-profile-bot/releases) 下载 `wechat-profile-bot-v2.3.1.zip`，解压后得到：

```
wechat-profile-bot-linux-amd64            Linux 服务端（amd64）
wechat-profile-bot-windows-amd64.exe      Windows 服务端（amd64）
wechat-profile-bot.service                Linux systemd 服务模板
start.bat                                 Windows 前台运行
install-service.bat                       Windows 安装为服务（需 nssm.exe）
uninstall-service.bat                     Windows 卸载服务
config.json                               配置模板
README.md                                 本文档
```

- Linux 服务器用 `wechat-profile-bot-linux-amd64`，Windows 用 `.exe`
- Docker 部署见下方「Docker 部署」，需要源码（`git clone` 本仓库）

> 也可自行编译，需要 Go 1.25+：
> ```bash
> go build -ldflags="-s -w" -o wechat-profile-bot .                                  # 当前平台
> CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o wechat-profile-bot-linux-amd64 .
> CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o wechat-profile-bot-windows-amd64.exe .
> ```

### 2. 配置

首次运行会在程序目录生成 `config.json`，编辑以下内容：

```json
{
  "myName": "你的微信昵称",
  "apiPort": 17965,
  "apiToken": "",
  "apiWhitelist": [],
  "trustedProxies": [],
  "llm": {
    "apiKey": "sk-xxx",
    "baseURL": "https://api.deepseek.com",
    "model": "deepseek-chat",
    "disableThinking": true
  },
  "profile": {
    "coldStartCount": 20,
    "updateInterval": 10
  }
}
```

| 字段 | 含义 |
|------|------|
| `myName` | 你自己的微信昵称，必须与微信里显示的完全一致 |
| `apiPort` | REST API 端口，供 Windows 桌面版远程调用。默认 `17965`（非常用端口，降低被扫描风险）；填 `-1` 则完全禁用 API 服务 |
| `apiToken` | API 认证令牌（Bearer Token）。**留空则首次启动自动生成一个随机令牌并写回本文件**，之后保持不变 |
| `apiWhitelist` | IP 白名单，支持单 IP 与 CIDR，如 `["1.2.3.4", "192.168.1.0/24"]`。空数组 = 不限制。**反向代理后部署时这里仍填访客的真实公网 IP**，配合下面的 `trustedProxies` 使用 |
| `trustedProxies` | 可信反向代理的 IP/CIDR，如 `["127.0.0.1"]`、`["10.0.0.0/8"]`。仅当 TCP 直连来源命中此列表时，白名单/封禁/限流才按 `X-Forwarded-For` 里的真实访客 IP 判定；直连来源不命中时该头一律忽略（防伪造）。不经过反代请留空，切勿填写 `0.0.0.0/0` |
| `llm.apiKey` | 大模型 API Key，默认 DeepSeek，也可换成任意 OpenAI 兼容接口 |
| `llm.baseURL` | 接口地址，不带末尾斜杠 |
| `llm.model` | 模型名 |
| `llm.disableThinking` | 是否关闭模型的推理思考模式，默认 `true`。默认开启思考的模型（deepseek-v4-flash、qwen3.8-flash 等）必须保持 `true` |
| `profile.coldStartCount` | 累计多少条对方消息后首次生成画像，默认 20 |
| `profile.updateInterval` | 之后每新增多少条对方消息更新一次画像，默认 10 |

### 3. 运行

```bash
./wechat-profile-bot-linux-amd64
```

首次运行会打印二维码链接，用手机微信扫码并确认绑定。

### 4. 使用

在微信里向 ClawBot 发送：

- **粘贴聊天记录** → 自动识别并存入，返回分析结果
- **帮助** → 查看所有命令
- **画像 昵称** → 查看联系人画像
- **列表** → 查看所有联系人

### 5. 网页管理界面

浏览器打开 `http://服务器IP:17965/`，输入 `config.json` 里的 `apiToken` 登录。
首次登录需绑定 TOTP 验证器（Google/Microsoft Authenticator、微信、支付宝均可），以后每次登录输入动态码。

功能包括：联系人列表、画像查看/编辑、消息记录、历史版本、统计、合并/撤销、**备份导出/导入**。

## 常驻运行与开机自启

> **建议顺序**：先前台跑一次完成扫码绑定（二维码链接直接打印在终端，不会写进日志文件），拿到 `ilink_credentials.json` 之后再转成常驻服务。凭据会一直复用，之后重启都不用再扫码。

### Linux：systemd 服务（推荐）

**1. 放置程序并建专用用户**

```bash
sudo mkdir -p /opt/wechat-profile-bot
sudo cp wechat-profile-bot-linux-amd64 /opt/wechat-profile-bot/wechat-profile-bot
sudo chmod +x /opt/wechat-profile-bot/wechat-profile-bot

# 用低权限专用用户运行，不要用 root
sudo useradd -r -s /usr/sbin/nologin -d /opt/wechat-profile-bot wpbot
sudo chown -R wpbot:wpbot /opt/wechat-profile-bot
```

**2. 前台跑一次，完成配置和扫码**

```bash
cd /opt/wechat-profile-bot && sudo -u wpbot ./wechat-profile-bot
```

第一次会生成 `config.json` 模板并退出，填好 `llm.apiKey`、`myName` 后再执行一次，用手机微信扫描终端里的二维码链接，看到登录成功后 `Ctrl+C` 退出。

**3. 创建服务单元** `/etc/systemd/system/wechat-profile-bot.service`

仓库里带了现成模板（源码在 `scripts/linux/wechat-profile-bot.service`，发布包内为 `wechat-profile-bot.service`）：

```bash
sudo cp scripts/linux/wechat-profile-bot.service /etc/systemd/system/
```

内容如下，也可以自己手写一份：

```ini
[Unit]
Description=WeChat Profile Bot
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=wpbot
Group=wpbot
WorkingDirectory=/opt/wechat-profile-bot
ExecStart=/opt/wechat-profile-bot/wechat-profile-bot
Restart=always
RestartSec=10
LimitNOFILE=65536

# 安全加固：不给额外权限、独立 /tmp、系统目录只读
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

> `WorkingDirectory` 必须是程序所在目录：`config.json`、数据库、`bot.log`、`security.log`、`banned_ips.json` 全部按「与数据库同目录」存放，靠工作目录定位。
> `ProtectHome=true` 会让 `/home` 不可访问，所以**不要把程序装在 `/home` 下**；确实要放 `/home`，就把这一行删掉。

**4. 启用并设为开机自启**

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now wechat-profile-bot
sudo systemctl status wechat-profile-bot
```

**日常管理**

| 操作 | 命令 |
|------|------|
| 启动 / 停止 / 重启 | `sudo systemctl start\|stop\|restart wechat-profile-bot` |
| 开 / 关开机自启 | `sudo systemctl enable\|disable wechat-profile-bot` |
| 查看状态 | `sudo systemctl status wechat-profile-bot` |
| 实时看运行日志 | `sudo journalctl -u wechat-profile-bot -f` |
| 看最近 200 行 | `sudo journalctl -u wechat-profile-bot -n 200 --no-pager` |
| 看业务日志 | `tail -f /opt/wechat-profile-bot/bot.log` |
| 看安全日志（登录失败/封禁/限流） | `sudo tail -f /opt/wechat-profile-bot/security.log` |
| 查看被封 IP | `sudo -u wpbot /opt/wechat-profile-bot/wechat-profile-bot --list-bans` |
| 解封 IP | `sudo -u wpbot /opt/wechat-profile-bot/wechat-profile-bot --unban 1.2.3.4` 后 `sudo systemctl restart wechat-profile-bot` |

> **需要重新扫码时**（换了微信号、凭据被删）：`sudo systemctl stop wechat-profile-bot`，再按第 2 步前台跑一次，扫完码 `Ctrl+C`，然后 `sudo systemctl start wechat-profile-bot`。服务模式下二维码链接也会进 `journalctl`，属敏感信息，看完别外传。

### Linux：不用 systemd 的后台运行

```bash
cd /opt/wechat-profile-bot
nohup ./wechat-profile-bot > /dev/null 2>&1 &
```

程序自己会写 `bot.log` 和 `security.log`，所以标准输出可以直接丢弃。

```bash
pgrep -af wechat-profile-bot              # 查进程
kill $(pgrep -f wechat-profile-bot)       # 停止
```

开机自启（没有 systemd 的老系统）：把下面这行加到 `/etc/rc.local` 的 `exit 0` 之前，并 `chmod +x /etc/rc.local`

```bash
cd /opt/wechat-profile-bot && nohup ./wechat-profile-bot >/dev/null 2>&1 &
```

也可以用 `tmux` / `screen` 挂一个会话，方便随时 attach 回来看输出。

### Windows：安装为服务（nssm）

仓库 `scripts/windows/` 下带了三个脚本（发布包里直接和 exe 放在一起），Windows 本身不能把 exe 直接注册成服务，需要 [nssm](https://nssm.cc/download)：

1. 下载 nssm，解压后把 `win64/nssm.exe` 放到 exe 所在目录
2. **先双击 `start.bat` 前台完成首次扫码**（二维码链接只在这个窗口里，不写日志），登录成功后关掉窗口
3. 双击 `install-service.bat`，自动完成：服务名 `WeChatProfileBot`、开机自启、崩溃后 10 秒重启、`bot.log`/`bot-error.log` 单文件超 10MB 自动轮转
4. 日常管理：

```bat
net start WeChatProfileBot
net stop  WeChatProfileBot
sc query  WeChatProfileBot
```

卸载服务运行 `uninstall-service.bat`。

**不想装服务的话**：

```powershell
# 后台无窗口运行（PowerShell，在程序目录下执行）
Start-Process -FilePath ".\wechat-profile-bot-windows-amd64.exe" -WorkingDirectory (Get-Location) -WindowStyle Hidden

# 停止
Get-Process wechat-profile-bot-windows-amd64 | Stop-Process
```

开机自启（不装服务）：打开「任务计划程序」→ 创建任务 →
- 常规：勾选「不管用户是否登录都要运行」
- 触发器：新建 →「启动时」
- 操作：新建 → 程序填 exe 完整路径，**「起始于」必须填 exe 所在目录**（否则找不到 `config.json`）

## Docker 部署

镜像未发布到 Docker Hub，需要先本地构建。

### 方式一：docker compose（推荐）

在项目目录执行，compose 会自动本地构建镜像并启动：

```bash
docker compose up -d --build
```

### 方式二：手动构建并运行

```bash
docker build -t wechat-profile-bot:latest .

docker run -d --name wechat-profile-bot --restart unless-stopped \
  -v $(pwd)/config:/config \
  -e TZ=Asia/Shanghai \
  -p 17965:17965 \
  wechat-profile-bot:latest
```

配置和数据保存在 `./config` 目录。首次启动会在该目录生成 `config.json` 模板并退出，填好配置后再启动一次；启动日志里会打印二维码链接，用手机微信扫码绑定。

### 容器不再以 root 运行

镜像通过 `docker-entrypoint.sh` 启动：先在 root 权限下把 `/config` 的属主对齐到 `PUID:PGID` 并收紧到 `700`，再用 `su-exec` 降权运行业务进程。这样容器被攻破时也拿不到 root，同时宿主机上的 `./config` 仍然可读写。

`docker-compose.yml` 里可以调整：

```yaml
environment:
  - PUID=1000   # 改成宿主机上执行 docker 的用户的 id -u
  - PGID=1000   # 改成 id -g
```

- 默认 `1000:1000`，与大多数 Linux 首个普通用户一致；用 `id -u` / `id -g` 查自己的值
- 启动时会自动 `chown`，所以从旧版本（root 运行）升级过来不需要手工处理已有文件
- 设 `PUID=0` 可退回旧行为（以 root 运行），作为排查问题时的逃生通道
- 手动 `docker run` 时同理，加 `-e PUID=$(id -u) -e PGID=$(id -g)` 即可

### Docker 日常运维

| 操作 | 命令 |
|------|------|
| 启动（含构建） | `docker compose up -d --build` |
| 停止 / 重启 | `docker compose stop` / `docker compose restart` |
| 看容器输出 | `docker compose logs -f`（`--tail 200` 只看最近 200 行） |
| 看业务日志 | `tail -f ./config/bot.log` |
| 看安全日志 | `tail -f ./config/security.log` |
| 查看被封 IP | `docker exec wechat-profile-bot /app/wechat-profile-bot --list-bans` |
| 解封 IP | `docker exec wechat-profile-bot /app/wechat-profile-bot --unban 1.2.3.4` 然后 `docker compose restart` |
| 升级到新版 | 替换源码后 `docker compose up -d --build`（数据在 `./config`，不会丢） |
| 备份全部数据 | 直接打包宿主机 `./config` 目录，或用网页端「备份」导出 zip |

**开机自启**：`docker-compose.yml` 里的 `restart: unless-stopped` 已经保证容器随 Docker 启动，还需要让 Docker 自身开机自启：

```bash
sudo systemctl enable docker
```

**关于端口**：`docker-compose.yml` 用的是 `network_mode: host`（避免 bridge 网络访问外部大模型 API 的问题），此时 `-p` / `ports` 映射**不生效**，容器直接监听宿主机的 17965。改用 bridge 网络时才需要 `-p 17965:17965`。

**首次扫码**：`docker compose logs -f` 里会打印二维码链接，用手机微信打开绑定即可。二维码链接等同登录凭据，别截图外传。

## 命令列表

| 命令 | 说明 |
|------|------|
| 帮助 | 显示帮助 |
| 列表 | 查看所有联系人 |
| 画像 [昵称] | 查看画像（不填显示最近更新的） |
| 统计 昵称 | 查看消息统计 |
| 备注 昵称 内容 | 设置联系人备注 |
| 补充 昵称 信息 | 手动补充画像信息 |
| 备份 | 在服务器生成完整备份文件（下载/导入用网页管理界面） |
| 合并 新昵称 旧昵称 | 合并联系人 |
| 撤销合并 昵称 | 撤销最近一次合并 |
| 合并记录 | 查看合并历史 |
| 删除 昵称 | 删除联系人（需二次确认） |
| 状态 | 查看登录、运行状态和服务器磁盘/内存（磁盘将满会提醒备份迁移） |
| 重登 | 重置登录状态（需重启） |

## 网页管理界面（含双因素认证）

浏览器打开 `http://服务端IP:17965/` 即可使用网页版管理界面（功能与桌面端对齐）。登录采用双因素：

1. 输入 `config.json` 里的 `apiToken`
2. 首次登录自动弹出二维码，用 Google/Microsoft Authenticator、微信、支付宝等 TOTP 验证器扫码绑定并输入 6 位码确认；以后每次登录输入验证器上的动态码（30 秒变化）

说明：

- 浏览器登录后只保存有期限（7 天）的**网页会话令牌**，不长期保存 `apiToken`；退出即吊销
- Windows 桌面端远程模式继续直接用 `apiToken`，不受影响
- TOTP 密钥保存在数据目录的 `totp_secret.json`（Docker 下为 `/config/totp_secret.json`，权限 0600）。**换手机或验证器丢失时，在服务器删除该文件**，下次网页登录会重新走绑定流程

## 桌面端远程对接（REST API）

服务端启动后会在 `apiPort`（默认 17965）监听 `/api/`，Windows 桌面版 [wechat-profile](https://github.com/caodabao99/wechat-profile)
可以改用远程模式复用这份数据——桌面端界面不变，数据和 LLM 分析都在服务端完成。

桌面端 `config.json`：

```json
{
  "myName": "你的微信昵称",
  "remote": {
    "enabled": true,
    "apiURL": "http://服务端IP:17965",
    "apiToken": "服务端 config.json 里的 apiToken"
  }
}
```

远程模式下桌面端不需要 `llm.apiKey`。启动时会先请求 `/api/status` 探活，连不上直接弹窗退出。

### 安全模型

按顺序生效，任一层不通过立即返回：

1. **非常用端口**：默认 17965，降低被批量扫描的概率；不需要远程访问时把 `apiPort` 设为 `-1` 彻底关闭
2. **IP 黑名单**：登录失败累计 10 次的 IP 会被**永久封禁**，之后所有请求直接 403（详见下方「登录失败封禁」）
3. **IP 白名单**（`apiWhitelist`）：支持单 IP 和 CIDR，**优先于 Token 校验**，不在名单内一律 403；空数组表示不限制。反代后部署配合 `trustedProxies` 按真实访客 IP 判定（见下方「反向代理部署」）
4. **Bearer Token**（`apiToken`）：所有接口（含 `status`）都要求 `Authorization: Bearer <token>`，缺失或不匹配返回 401
5. **接口限流**：`/api/ingest` 每 IP 每分钟最多 120 次，防止 Token 泄露后被脚本刷爆大模型账单
6. **安全响应头**：所有响应自动带 `X-Content-Type-Options`、`X-Frame-Options: DENY`、`Referrer-Policy`、`Content-Security-Policy`，防点击劫持与 MIME 嗅探
7. **HTTP 超时**：`ReadHeaderTimeout 10s` + `IdleTimeout 120s`，避免慢速连接（Slowloris）长期占满连接数

> 服务端暴露在公网时务必同时配置 `apiToken` 和 `apiWhitelist`，或用防火墙/安全组限制来源。

### 登录失败封禁

网页登录（Token + TOTP）失败会按来源 IP 计数，**同一 IP 累计失败 10 次即永久封禁**，封禁后该 IP 访问任何接口都返回 403，重启服务也不会解除。

所有失败与封禁事件单独记在数据目录的 `security.log`（权限 0600，与业务日志 `bot.log` 分开），封禁名单持久化在 `banned_ips.json`（权限 0600，原子写入；文件损坏时自动备份为 `.corrupt` 并以空名单启动）。

登录成功会把该 IP 的失败计数清零，所以正常使用不会被误封。

服务端的运维命令（执行完立即退出，不会启动服务，服务在跑也可以直接执行）：

```bash
./wechat-profile-bot --list-bans      # 查看当前被封禁的 IP
./wechat-profile-bot --unban 1.2.3.4  # 解封指定 IP
./wechat-profile-bot --unban-all      # 解封全部
./wechat-profile-bot --help           # 查看全部命令
```

Docker 下把命令换成 `docker exec wechat-profile-bot /app/wechat-profile-bot --list-bans`。

> **解封后需要重启服务才生效**：这些命令只改 `banned_ips.json`，正在运行的进程把名单读在内存里，不会自动重新加载。`docker restart wechat-profile-bot` 或重跑一次二进制即可。

> 客户端 IP 默认只取自 TCP 连接的 `RemoteAddr`；配置了 `trustedProxies` 后才会采纳可信代理转发的 `X-Forwarded-For`，且从代理链最右端逐跳校验，伪造的左侧 IP 无法绕过封禁与白名单。

### 反向代理部署（Nginx / Caddy 等）

直接在反代后启用 `apiWhitelist` 会遇到两个问题：服务端看到的来源 IP 是反代服务器（导致所有人都被拦）；而把反代 IP 加进白名单又等于不设防（任何人都能访问反代）。正确做法是**白名单照填真实访客 IP，另把反代自身登记为可信代理**：

1. `config.json` 配置：
   ```json
   "apiWhitelist": ["你的公网IP"],
   "trustedProxies": ["127.0.0.1"]
   ```
   反代与本程序不在同一台机器时，`trustedProxies` 填反代的内网 IP 或网段（如 `["10.0.0.0/8"]`），不要填公网大网段。Docker 部署时容器看到的直连来源通常是网桥网关，同机反代可填 `["172.16.0.0/12", "127.0.0.1"]`；反代也在容器里则填它所在的 compose 网络网段。
2. Nginx 站点配置必须转发访客 IP（程序按 `X-Forwarded-For` 链逐跳校验，伪造头无效）：
   ```nginx
   location / {
       proxy_pass http://127.0.0.1:17965;
       proxy_set_header Host $host;
       proxy_set_header X-Real-IP $remote_addr;
       proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
       proxy_set_header X-Forwarded-Proto $scheme;
   }
   ```
3. 验证：登录网页管理界面，打开「状态」页，「服务运行」卡片里的「识别到的你的 IP」应显示你的公网 IP（而不是反代 IP），旁边显示「白名单已启用 / 反代可信」。
4. 安全建议：反代加 HTTPS；有条件时用防火墙让 17965 端口只允许反代服务器访问，避免访客绕过反代直连（直连时其 `X-Forwarded-For` 伪造头会被忽略，但多层防护更稳妥）。

### 接口一览

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/status` | 服务状态与联系人数（也需认证） |
| GET | `/api/contacts?includeMerged=1` | 联系人列表（全量，桌面端兼容） |
| GET | `/api/contacts?paged=1&offset=0&limit=30&q=关键词` | 联系人分页列表（返回 `{list,total,offset,limit}`） |
| POST | `/api/contacts` | 创建联系人，body `{"name"}` |
| POST | `/api/contacts/resolve` | 昵称解析为 ID，body `{"name"}` |
| GET / DELETE | `/api/contacts/{id}` | 联系人详情 / 删除联系人 |
| GET | `/api/contacts/{id}/messages?offset=&limit=` | 消息分页（默认 limit 50） |
| GET | `/api/contacts/{id}/stats` | 消息统计 |
| GET | `/api/contacts/{id}/history?limit=` | 画像历史 |
| POST | `/api/contacts/{id}/remark` | 设置备注，body `{"remark"}` |
| POST | `/api/contacts/{id}/name` | 改名，body `{"name"}` |
| POST | `/api/contacts/{id}/supplement` | 手动补充画像，body `{"note"}` |
| POST | `/api/contacts/{id}/regenerate` | 重新生成画像 |
| POST | `/api/contacts/{id}/analyze` | 意图分析，body `{"message"}` |
| POST | `/api/merge` | 合并联系人，body `{"sourceId","targetId","useSourceName","regenerate"}` |
| POST | `/api/merge/undo` | 撤销合并，body `{"logId"}` |
| GET | `/api/merge/logs?limit=&targetId=` | 合并记录 |
| POST | `/api/ingest` | 提交聊天记录识别，body `{"text","analyze"}` |

验证示例：

```bash
curl -H "Authorization: Bearer <token>" http://127.0.0.1:17965/api/status
```

## 备份与恢复（换服务器/换电脑迁移）

在网页管理界面顶部点「备份」页：

- **导出备份**：下载一个 `wechat-profile-backup-日期.zip`，内含全部联系人、消息、画像历史、合并记录，以及 `config.json`、`ilink_credentials.json`、`totp_secret.json`（网页登录态 `web_sessions.json` 刻意不包含）
- **导入恢复**：选择之前导出的 zip，当前数据库会被整体替换；恢复前自动在数据目录留一份 `auto-backup-pre-restore-时间.zip`，恢复配置文件后需重启服务才生效
- **操作记录**：页面下方显示最近 50 次备份/恢复操作（时间、来源端、文件名、结果），失败操作标红

微信里发送「备份」命令可在服务器数据目录直接生成一份备份文件（适合无浏览器时先落盘，再用网页端下载）。桌面端连接本服务时，也可以直接用桌面端窗口左下角的「备份…/恢复…」按钮完成同样的操作（远程模式自动调用本服务接口）。

### 备份加密（可选）

导出时可以在「备份密码」框里填一个口令，填了就把 zip 里的**密钥文件**加密：

| | 说明 |
|---|---|
| 加密范围 | 只加密 `config.json`、`ilink_credentials.json`、`totp_secret.json`（模型 API Key、微信登录凭据、2FA 密钥） |
| 不加密 | `data.db`（聊天记录）和 `MANIFEST.json` 始终是明文，zip 也仍是标准格式，可用任意解压软件打开查看 |
| 加密算法 | PBKDF2-HMAC-SHA256 派生密钥（120000 次迭代）+ AES-256-GCM，文件在 zip 内改名为 `原名.enc` |
| 留空 | 和以前完全一样，明文保存，任何机器都能直接导入 |
| 导入 | 程序会自动识别备份是否加密；加密的必须填同一口令，口令不对会**直接中止恢复，现有数据不受影响** |

> **口令不会被程序保存在任何地方**，忘了就再也解不开那几个 `.enc` 文件（聊天记录不受影响，仍然可以正常恢复）。

对应接口：`POST /api/backup/export`，body `{"password":"..."}`；导入时在 multipart 里加 `password` 字段。`GET /api/backup/export` 仍然是明文备份（口令放查询串会进浏览器历史和访问日志，故不支持）。

> 备份 zip 包含模型 API Key、iLink 登录凭据和 TOTP 密钥，即使填了密码，聊天数据库仍是明文，请像保管密码一样妥善保管，不要通过不可信渠道传输。

## 注意事项

- 需要微信 8.0.70+，在「我 → 设置 → 插件」中开启 ClawBot
- 只支持私信（DM），不支持群聊
- 数据全部存在本地 SQLite，不上传到任何服务器
- 首次生成画像需要积累一定数量的对方消息（默认 20 条）

## 数据目录下的文件

均与数据库同目录（Docker 下为 `/config`）：

| 文件 | 说明 |
|------|------|
| `config.json` | 配置，含模型 API Key 和 `apiToken` |
| `wechat-profile-bot.db` | SQLite 数据库（联系人、消息、画像、备份记录） |
| `bot.log` | 运行日志 |
| `security.log` | **安全日志**：登录失败、IP 封禁、限流拒绝、白名单拒绝（权限 0600） |
| `banned_ips.json` | **永久封禁名单**，重启不丢失（权限 0600） |
| `ilink_credentials.json` | 微信登录凭据，删掉需重新扫码 |
| `totp_secret.json` | 网页登录的 2FA 密钥，删掉后下次登录重新绑定 |
| `web_sessions.json` | 网页会话令牌（7 天有效），刻意不进备份 |

这些都是运行时生成的，`.gitignore` 已排除，不要提交到仓库。

## 更新日志

### v2.3.1（2026-10-03）

体验修复与小增强，全部改动向后兼容，不影响既有数据和配置。**网页端更新后请强制刷新一次（Ctrl/Cmd + Shift + R）**，避免浏览器继续使用旧缓存。

**优化**

- **编辑画像与画像查看格式完全对齐**：网页端编辑弹窗改为与画像查看一致的 9 节结构（概要 / 基本信息 / 性格特征 / 沟通风格 / 兴趣爱好 / 情绪模式 / 关系 / 典型意图 / 重要事实），字段名与画像里逐字一致（职业、所在地、口头禅、表情使用、压力源、安慰有效话题、近期共同事件等），回填顺序固定，不再出现两套字段名和概要排到最后的问题
- **建议回复固定给四种风格**：意图分析每次返回「稳妥得体 / 简洁直接 / 亲切热情 / 委婉留余地」各一条，风格由用户自己挑，不再由模型只选 3 种
- **微信「状态」命令增加服务器资源**：显示磁盘、内存、系统负载和程序占用；数据盘使用 ≥70% 给出【提醒】、≥90% 给出【紧急】并提示尽快备份迁移，赶在写满前发现风险
- **全角竖线分隔符**：`改写`、`草稿检查` 命令的竖线 `｜` 和 `|` 都认，中文输入法下不用切换

**测试**

- 新增分隔符解析、建议回复数量上限、服务器资源块单测；画像编辑保存 round-trip 经浏览器实测

### v2.3（2026-10-03）

体验与部署改进，全部改动向后兼容，不影响既有数据和配置（使用反向代理需新增一项配置，见下）。

**新增**

- **网页「状态」页**：展示微信连接、服务运行时长、服务器资源（CPU / 内存 / 磁盘 / 系统负载）、本程序资源占用、数据量，每 5 秒自动刷新；同时显示服务端识别到的访问 IP 与白名单/可信代理状态，方便排查反代问题
- **联系人分页与服务端搜索**：列表默认每页 30 条、底部「加载更多」，不再首屏全量加载；搜索改为「查询」按钮/回车触发，由后端匹配昵称、备注、别名，未加载的联系人也能搜到。桌面端远程模式仍用全量接口，不受影响
- **反向代理真实 IP 支持**：新增 `trustedProxies` 配置项。只有 TCP 直连来源在可信代理列表内时才采纳 `X-Forwarded-For`，并从代理链最右侧逐跳校验，白名单 / 封禁 / 限流全部按真实访客 IP 判定；直连客户端伪造 XFF 无法绕过。反代后 `apiWhitelist` 继续填访客真实公网 IP 即可
- **微信端建议回复方便复制**：意图分析的建议回复改为逐条单独发送，长按单条即可复制；风格标签放在消息末尾（如 `内容【稳妥得体】`），粘贴后从末尾一删即可

**优化**

- 网页「发送前检查 + 候选回复 + 画像变化」拆为联系人详情的独立「辅助」页签，不再混在画像页；画像变化可收起
- 候选回复风格体系统一：意图分析自动按语境挑选 3 种风格并保证差异可感知，修复旧版模型返回字符串数组时风格错位的问题；微信 / 网页 / 桌面三端展示一致
- 网页手机端适配（≤640px）：顶栏导航与详情子页签横向滚动、弹窗收窄限高、状态页单列、联系人摘要两行截断、表格收紧
- 网页「命令说明」与微信端帮助文案逐字同步

**测试**

- 新增真实 IP 解析安全单测 14 例（直连、单级/多级反代、XFF 伪造、IPv6、CIDR 白名单）；分页与搜索经浏览器实测

### v2.2（2026-10-02）

功能增强与逻辑修复，全部改动向后兼容，不影响既有数据和部署方式。

**新增**

- **意图分析给多条候选回复**：一次分析结合语境从「稳妥得体 / 简洁直接 / 亲切热情 / 委婉留余地」四种固定风格中挑最合适的 3 种，每条带风格标签；网页/桌面端每条可单独复制。模型只回一条时按实际数量展示，不凑数
- **编辑画像**：网页/桌面端联系人详情新增画像编辑，可修改内容、删除列表条目、清空字段，直接保存不经过 AI 改写，改动记入画像历史；画像已被其他操作更新时会拒绝覆盖，避免丢失新内容
- **一键换个说法**：候选回复旁点选目标风格（稳妥得体 / 简洁直接 / 亲切热情 / 委婉留余地）即可改写该条，只改写该条、保留原意、不捏造事实。微信命令：`改写 昵称｜风格｜内容`（竖线 `｜`、`|` 都可以，不用切换输入法）
- **回复前帮我看看**：输入准备发送的草稿，结合画像和近期聊天指出可能的歧义并给出修改建议，不自动发送。微信命令：`草稿检查 昵称｜内容`（竖线中英文输入法都可以）
- **画像变化高亮**：对比最近两次画像，展示新增 / 修改 / 删除及前后内容，不调用模型。微信命令：`画像变化 昵称`

**修复（11 项逻辑问题）**

- 恢复备份：修复特定并发下的数据库死锁
- 撤销合并：新合并保存必要快照，撤销时恢复源联系人的消息与原画像（旧版本合并时已删除且无快照的消息无法找回）
- 后台画像任务：合并、撤销或恢复前启动的过期结果不再写回，避免画像污染
- 2FA：绑定检查与写入原子化，已绑定时拒绝覆盖，并发首次绑定仅一次成功
- 备份恢复：报失败时回滚数据库并补偿文件，不再出现"已替换成功却报失败"
- 删除确认：恢复备份后清除旧确认，不再出现"确认删 A 实际删到 B"
- 连字符日期（如 `2025-6-10`）正确解析，不再回退当前时间导致去重失效
- 意图分析失败后重置去重标记，同一文本可直接重试
- 网页刷新 `#/contact/ID` 链接后正常加载详情，不再空白
- 桌面端备份密码对话框关闭前不再丢失输入；删除最后一个联系人后清空右侧面板

**说明**

- 新功能（改写 / 草稿检查 / 画像变化）复用现有模型接口，不新增外部依赖，配置无需改动
- 画像编辑针对当前画像，旧历史仍保留；后续重新分析聊天记录时模型可能再次提取已删除的信息，界面已有提示

### v2.1（2026-10-02）

安全加固专项，全部改动向后兼容，不影响既有数据和用法。

**新增**

- **登录失败永久封禁**：同一 IP 网页登录失败累计 10 次即永久封禁，之后所有请求 403，重启不解除。名单持久化在 `banned_ips.json`（0600，原子写入，损坏时自动备份为 `.corrupt` 并以空名单启动）
- **独立安全日志 `security.log`**（0600）：记录每次登录失败的 IP 与原因、封禁/解封事件、限流拒绝、白名单拒绝；与业务日志 `bot.log` 分开，便于审计
- **封禁自救命令**：`--list-bans`、`--unban <ip>`、`--unban-all`、`--help`（改完名单需重启服务生效）
- **备份加密（可选）**：导出时可设口令，把 zip 内的密钥文件（`config.json`、`ilink_credentials.json`、`totp_secret.json`）用 AES-256-GCM 加密为 `.enc`；口令不设则完全等同旧行为。聊天记录 `data.db` 与 `MANIFEST.json` 始终明文，zip 仍是标准格式。导入自动识别，口令错误直接中止、不动现有数据
- **`/api/ingest` 限流**：每 IP 每分钟 120 次，超出返回 429 + `Retry-After`，防止 token 泄露后被刷爆大模型账单
- **安全响应头**：所有响应带 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`Content-Security-Policy`
- **HTTP 超时**：`ReadHeaderTimeout 10s`、`IdleTimeout 120s`，防慢速连接（Slowloris）占满连接数
- **Docker 容器不再以 root 运行**：新增 `docker-entrypoint.sh`，先对齐 `/config` 属主并收紧到 700，再用 `su-exec` 降权到 `PUID:PGID`（默认 1000:1000）；`PUID=0` 可退回 root
- **部署脚本入库**：`scripts/linux/wechat-profile-bot.service`（systemd 模板）、`scripts/windows/{start,install-service,uninstall-service}.bat`（nssm 服务）
- README 补全「常驻运行与开机自启」（systemd / nohup / rc.local / Windows 服务 / 任务计划）和「Docker 日常运维」章节

**说明**

- 客户端 IP 默认取自 TCP `RemoteAddr`；配置 `trustedProxies` 后仅采纳可信代理链上的 `X-Forwarded-For`（配置方法见「反向代理部署」），伪造头无法绕过封禁与白名单
- 桌面端用 `apiToken` 直连，token 配错只记审计日志、**不计入封禁计数**，不会把自己锁死
- 备份口令程序不保存在任何地方，忘了就解不开 `.enc`（聊天记录不受影响）

### v2.0（2026-10-02）

首个服务端版本：微信消息收发、AI 人物画像与意图分析、联系人管理（备注/合并/撤销）、网页管理界面（2FA）、REST API 供桌面端远程调用、数据备份/恢复与操作日志、Docker 部署支持。

## 关联项目

- [wechat-profile](https://github.com/caodabao99/wechat-profile) — Windows 桌面版（walk GUI）
