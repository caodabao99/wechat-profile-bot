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
- **关系助手**（网页端，默认关闭）：重要日子提醒、久未联系提醒、亲密度评分、AI 情绪预警，每日提醒 / 每周报告通过 SMTP 邮件发送
- **每周维护计划**（网页看板）：每周一自动排行“本周最该联系的 5 个人”并草拟开场白，只在看板展示不发邮件，可手动立即重算
- **隐式反馈闭环**：粘贴记录入库时自动识别你已联系→标建议已执行，14 天后自动回测互动是否回暖，看板展示近 90 天采纳率/回暖率，全程零手动操作
- **关系图谱**（洞察页）：从画像交叉比对共同城市/同类职业/共同兴趣/提到彼此，纯 SQL 推导谁和谁可能认识及可信度
- **人生总览**（洞察页，人生模拟器）：把每段关系算成一份“人际资产账本”——余额（关系深度）、现金流（近期互动相对历史的增值/缩水）、折旧（断联天数 + 冷却斜率）、风险分与象限分类，并汇总人际总财富、依赖集中度、高风险将断清单与时间精力回流偏差。纯 SQL 零模型开销，每周一自动算好，打开即看
- **未来推演**（洞察页，人生模拟器）：按当前互动趋势把每段关系确定性地快进到 90 天后，算出“什么都不做会断掉几段”；系统自动生成三条 what-if 对比（维持现状 / 每周联系高风险清单一分钟 / 把时间往家人身上移），每条附“可多保住几段 / 总财富变化”，人只看结果不拨旋钮
- **人生年表**（洞察页，人生模拟器）：从最早对话、按年活跃度、跨年最长情的关系、最近在意的人与你手动记下的大事，自动聚合出里程碑年表；数据稀疏时优雅降级为空，不强推不乱预测
- **待跟进事项**：手动记一笔，或让 AI 从最近聊天里扫出「答应过的事 / 借钱还钱 / 待回复」，未完成项每天随提醒邮件一起推送
- **联系人标签**：给联系人打自定义标签分组，列表页可按标签筛选、勾选多人批量打标
- **聊天记录全文搜索**：跨全部联系人按关键词搜消息，可限定联系人、时间范围，可选一并搜归档消息
- **联系人时间线**：单个联系人的完整往来脉络（首次联系、画像变更、合并、手动记录的大事），可手动补记事件
- **对话预演**：把要跟某个人开口的重要对话（谈涨薪、拒绝借钱、表白等）先练一遍——AI 按这个人的画像和真实聊天语气扮演对方跟你对练，随时能看到自己话术带来的情绪变化，结束后以沟通顾问身份复盘打分。**演练内容不落库、也不会真的发出去**
- **重要日子日历订阅**：把联系人生日/纪念日导出成标准 `.ics` 订阅链接，手机或电脑日历客户端直接订阅，全年自动重复提醒
- **AI 祝福语草稿**：重要日子临近时按画像和你的说话风格生成 3 条草稿，自己复制粘贴发送（不代发）
- **疑似重复联系人推荐**：按昵称/备注相似度扫出可能是同一个人的联系人对，给出建议保留项，一键跳去合并
- **年度关系报告**：一键生成某一年的关系长页（消息量、最活跃的一天、月度走势、情绪曲线、亲密度变化、关键词、大事记），可分享
- **我的社交大盘**：全量联系人的互动统计——24 小时活跃分布、周几分布、双方回复速度、谁先开口、高频用词
- **消息归档**：超过保留期（默认 730 天 ≈ 2 年）的旧消息可一键或每日自动移入同库归档表，缩小活动数据、加快日常查询；随时可按联系人或全部恢复，随备份一起导出
- **可信设备（免登录）**：登录时勾选「信任此设备」，该浏览器 90 天内打开网页免输 Token 和动态验证码（使用即自动续期）；可在网页端查看、单独或全部吊销

## 快速开始

### 1. 获取程序

从 [Releases](https://github.com/caodabao99/wechat-profile-bot/releases) 下载 `wechat-profile-bot-v4.3.0.zip`，解压后得到：

```
wechat-profile-bot-linux-amd64            Linux 服务端（amd64）
wechat-profile-bot-windows-amd64.exe      Windows 服务端（amd64）
wechat-profile-bot.service                Linux systemd 服务模板
start.bat                                 Windows 前台运行
install-service.bat                       Windows 安装为服务（需 nssm.exe）
uninstall-service.bat                     Windows 卸载服务
Dockerfile                                Docker 镜像构建文件
docker-compose.yml                        Docker compose 配置
docker-entrypoint.sh                      Docker 容器入口脚本（降权启动）
config.json                               配置模板
README.md                                 本文档
```

- Linux 服务器用 `wechat-profile-bot-linux-amd64`，Windows 用 `.exe`
- Docker 部署见下方「Docker 部署」：直接加载 Release 附带的镜像 tar（`wechat-profile-bot-docker-v4.3.0.tar.gz`），或用包内 Dockerfile 本地构建，均不需要 git clone 源码

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

功能包括：联系人列表、画像查看/编辑、消息记录、历史版本、统计、合并/撤销、**备份导出/导入**、**关系助手**（重要日子 / 久未联系 / 亲密度 / 情绪预警看板与邮件提醒配置，默认关闭）。

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

镜像未发布到 Docker Hub，两种方式任选：

- **加载 Release 附带的镜像 tar**（推荐，无需 Go 环境）：下载 `wechat-profile-bot-docker-v4.3.0.tar.gz` 后 `docker load -i wechat-profile-bot-docker-v4.3.0.tar.gz`，得到 `wechat-profile-bot:v4.3.0` 镜像，再按下文 compose（删掉 `build:` 段）或 `docker run` 启动
- **本地构建**：需要源码或 Release 包内的 Dockerfile

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
- 顶部导航的 **洞察** 页聚合四个子面板：聊天记录全文搜索、年度关系报告（可另存分享）、我的社交大盘、疑似重复联系人推荐
- 联系人列表支持按标签筛选、勾选多人批量打标；联系人详情页新增 **时间线** 子页，可手动补记大事
- 联系人详情页的 **预演** 子页可以「对话预演」：描述一个场景后 AI 照画像扮演对方跟你对练，可选择谁先开口，随时点「结束并复盘」拿到话术点评与达成可能评分；整场演练只存在浏览器内存里，刷新或切换联系人即清空
- **关系助手** 页除每日提醒外，还包含待跟进事项（手动记 / AI 扫描）、重要日子 AI 祝福语草稿、日历订阅（`.ics`）密钥管理

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
| GET | `/api/search/messages?q=&contactId=&from=&to=&archive=1&offset=&limit=` | 全文搜索消息（`archive=1` 一并搜归档） |
| GET / POST | `/api/tags` | 标签列表 / 新建标签，body `{"name"}` |
| POST | `/api/tags/batch` | 批量打标，body `{"contactIds":[],"tagIds":[],"remove":false}` |
| PUT / DELETE | `/api/tags/{id}` | 重命名（body `{"name"}`）/ 删除标签 |
| GET / PUT | `/api/contacts/{id}/tags` | 读取 / 覆盖式设置某联系人的标签，body `{"tagIds":[]}` |
| GET / POST | `/api/contacts/{id}/timeline` | 联系人时间线 / 手动补记事件，body `{"title","detail","eventTime"}` |
| DELETE | `/api/contacts/{id}/timeline/{eventId}` | 删除一条手动记录的事件 |
| GET | `/api/contacts/{id}/rehearsal/context` | 对话预演的扮演依据（画像要点 + 真实聊天说话样例） |
| POST | `/api/contacts/{id}/rehearsal/turn` | 让 AI 以对方身份回一句，body `{"scene","turns":[{"role":"me\|other","text"}]}` |
| POST | `/api/contacts/{id}/rehearsal/review` | 结束预演并复盘，body 同上，返回 `{"summary","good","bad","risks","suggestions","score"}` |
| GET | `/api/insights/duplicates` | 疑似重复联系人推荐 |
| GET | `/api/insights/social?days=30` | 我的社交大盘 |
| GET | `/api/insights/report?year=2026&format=html` | 年度关系报告（`format=html` 返回可分享长页） |
| GET / POST | `/api/assistant/followups?status=open&limit=100` | 待跟进列表 / 手动新增，body `{"contactId","kind","content","amount"}` |
| POST | `/api/assistant/followups/scan` | 用 AI 从最近聊天里扫待跟进，body `{"contactId"}`（可选，缺省按每日上限自动挑） |
| PUT / DELETE | `/api/assistant/followups/{id}` | 改状态（body `{"status":"done\|ignored\|open"}`）/ 删除 |
| GET / POST | `/api/assistant/calendar/key` | 读取（打码）/ 重置 `.ics` 订阅密钥 |
| DELETE | `/api/assistant/calendar/key` | 清除订阅密钥（关闭日历订阅） |
| POST | `/api/assistant/blessing` | 生成 AI 祝福语草稿，body `{"contactId","kind","raw","month","day","dateStr","daysUntil"}`（除 `contactId` 外均可省略） |
| GET | `/api/calendar.ics?key=订阅密钥` | 日历订阅地址（不走 Bearer，仍受 IP 白名单约束） |
| GET / POST | `/api/assistant/weekly-plan` | 本周维护计划看板数据（排行+开场白+近90天反馈统计）/ 立即重算（调模型，后台执行） |
| GET | `/api/relationships/connections` | 关系图谱全量连线（表为空时自动重建，上限 500） |
| GET | `/api/contacts/{id}/connections` | 某联系人的关联（上限 100，按可信度降序） |
| POST | `/api/relationships/connections/rebuild` | 手动全量重建关系连线（纯 SQL，不调模型） |
| GET | `/api/life/state` | 人生总览快照（资产账本+组合聚合+时间回流；缓存缺失/过期则现算，纯 SQL） |
| GET | `/api/life/projection` | 未来推演（90 天走势 + 三条自动 what-if 策略） |
| GET | `/api/life/timeline` | 人生年表里程碑（每次现算，聚合很轻） |
| POST | `/api/life/recompute` | 后台异步重算人生状态+推演（立即返回，不阻塞） |

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
| `trusted_clients.json` | 可信设备令牌（90 天免登录，使用即续期），刻意不进备份，可在网页端吊销 |

这些都是运行时生成的，`.gitignore` 已排除，不要提交到仓库。

## 更新日志

### v4.3.0（2026-10-04）

把产品从“关系提醒工具”升级为“人生模拟器”：新增**人生总览 / 未来推演 / 人生年表**三个洞察子页，全部由系统自动算好、打开即看。升级只需替换二进制并重启，数据库结构向后兼容（新增两张派生表在启动时自愈创建，旧库无需迁移脚本）。

新增功能

- **人生总览**：把每段关系折算成一份“人际资产账本”——余额（关系深度）、现金流（近 30 天互动相对历史基线的增值/缩水）、折旧（断联天数 + 近 90 天冷却斜率）、风险分（陈旧 + 负趋势 + 主动性失衡合成）与象限分类（挚友/好友/熟人/弱关系/事务型）；并汇总人际总财富、本季涨跌、依赖集中度（HHI）、高风险将断清单，以及时间精力回流的类别占比偏差洞察（如“家人占比从 20% 降到 6%”）。纯 SQL 聚合现有数据，零模型开销
- **未来推演**：按当前互动趋势把每段关系确定性地快进到 90 天后（可解释、可复现、每条附“为什么”），算出“什么都不做→N 段关系将自然断掉”；系统自动生成三条 what-if 对比（维持现状 / 每周联系高风险清单一分钟 / 把时间往家人身上移），每条给出“可多保住 M 段 / 总财富 +X%”。人只看结果、不拨旋钮；数据不足（<30 天历史）的联系人标注“暂不预测”而非乱推
- **人生年表**：从最早一条对话、按年活跃度、跨年最长情的关系、最近在意的人与你手动记下的大事，自动聚合出里程碑年表；可选一句模型文案润色，未配模型时只出结构化里程碑、不报错

内部改进

- 新增两张派生缓存表（`life_state_cache`、`life_projection_cache`，DB `user_version` 升到 16），不纳入备份、恢复末尾清空、访问时缺则自愈重建，完全沿用周计划的骨架
- 周计划与人生模拟器共用周一触发点（零新增调度器），但各自独立加锁：周计划用 `lastWeekly` 周锁，人生模拟器用缓存新鲜度作天然周锁，互不影响幂等语义
- 人生状态/推演聚合在单连接池下均“先取尽结果再写库”，并在锁外取缓存，避免边读边写与嵌套取锁死锁
- 前端并入现有“洞察”页新增 3 个子页（不新建一级导航），设置卡加 `lifeSimEnabled` 开关（默认开，零配置）

> 提醒：人生模拟器全部能力纯本地规则计算、不调大模型、不产生意外费用；如不需要可在关系助手设置里关闭“启用人生模拟器自动计算”。

### v4.2.0（2026-10-04）

三个“系统主动做、人只看结果”的能力落地：关系助手从“给建议”升级为“每周排行 + 自动回测效果 + 看见人脉关联”。升级只需替换二进制并重启，数据库结构向后兼容（新增表在启动时自愈创建，旧库无需迁移脚本）。

新增功能

- **每周维护计划**：每周一调度器自动从活跃建议里算出“本周最该联系的 5 个人”（按优先级 + 亲密度排序），可用时由大模型草拟一句自然开场白，结果缓存在看板“本周维护计划”卡片直接展示。只在看板呈现、不发邮件；也可点“立即重算”手动刷新
- **隐式反馈闭环**：粘贴聊天记录入库时，若检测到你已经发过消息（sender=me），系统自动把对应建议标为“已执行”并记下执行前的互动趋势基线；14 天后自动回测互动是否回暖，得出改善/持平/恶化结论，看板底部展示“近 90 天建议采纳率与回暖率”。全程无需你点任何按钮
- **关系图谱**：从各联系人画像里交叉比对共同城市、同类职业、共同兴趣、以及“画像提到过彼此”，纯 SQL 推导出人际关联边（不额外调用模型）。洞察页新增“关系图谱”子页，展示谁和谁可能认识、可信度多高；结果表首次访问时缺则自愈重建

内部改进

- 新增两处派生表（周计划缓存、建议回测结果、关系连线）均不纳入备份，升级/恢复后自动重建，旧库平滑过渡
- 关系连线与周计划排行在单连接池下改为“先取尽结果再写库”，避免边读边写死锁

> 提醒：每周维护计划的开场白会调用大模型、按量产生费用；如不需要可在关系助手设置里关闭“启用每周维护计划”。

### v4.1.0（2026-10-04）

聚焦“关系助手到底怎么用”的一次体验收敛：去掉冗余的选择项，让助手开箱即是“主动帮你维护关系”的高灵敏行为。升级只需替换二进制并重启，数据库结构向后兼容。

功能调整（精简）

- **下线「运行模式」多预设选择器**：家人/好友/同事/客户/静音专注五套预设对大多数人是冗余负担。关系助手的定位本就是主动维护关系，因此直接将该功能整体移除（前后端与预设表），不再需要“先选模式”这一步
- **助手默认改为“真·客户档”高灵敏**：开箱即为沉寂 14 天即判降温、生日提前 5 天、情绪预警与待跟进 AI 自动抽取默认开启。总开关仍需你在配好 SMTP 后主动启用，避免未配置邮箱就后台空跑/产生模型费用
- **移除网页端「命令说明」页**：微信命令说明由微信端（发「帮助」）单一来源提供，网页端不再复制一份，避免两处维护不一致
- **关系助手页新增「三步上手」引导**：配邮箱→启用开关→看结果/收邮件的使用主线就地说明，功能不再“看不出怎么用”

> 提醒：情绪预警与待跟进自动抽取现在默认开启，会调用大模型、按量产生费用；不需要时可在关系助手页随时关闭。
> 旧版已创建的 `mode_presets` 表升级后保留不动，不影响备份/恢复。

> 详细变更与部署步骤见上文各功能章节。旧版本说明见下方 v4.0.0 及更早条目。

### v4.0.0（2026-10-04）

大版本更新：新增多项网页端能力、云端部署体验与全面的响应式适配，并修复一批跨端缺陷。升级只需替换二进制并重启，数据库结构向后兼容（新增表/列在首次访问时自愈创建，旧库无需迁移脚本）。

新增功能

- **运行模式预设**：把当前助手表单/阈值存成「客户 / 家人 / 朋友」等命名预设，一键切换不同关系的提醒风格；预设只存阈值不存密码/日历密钥，切换不会覆盖你的 SMTP 凭据
- **关系趋势与驾驶舱**：联系人维度的互动趋势、久未联系/冷却回暖判定与下一步行动建议集中呈现
- **「问 TA」**：基于已生成画像与真实聊天证据回答你关于某个人的具体问题，并附来源消息可点回看
- **周期性洞察报告**：支持按周/月/年多周期聚合生成可分享长页报告（年度/社交大盘延续增强）
- **云端公网访问地址自动探测**：未配置 `webBaseURL` 时自动探测服务器公网出口 IP 生成可点开的网页地址（`网址` 命令）；探测并发化、失败回退局域网 IP

稳定性与安全修复

- 模式预设应用会保留真实 SMTP 密码与日历密钥，不再被内置空凭据覆盖
- 网页助手设置保存时不再重置关系趋势阈值（静默天数/冷却/回暖），掩码凭据正确回填
- 分页查询补回消息 `id`，修复按 id 去重/定位偏差
- 网页登录响应/预设列表对密码与日历密钥一律打码，杜绝凭据外泄
- 助手调度器每周期加异常隔离，单次任务 panic 不再中断后续提醒
- 备份/恢复增强：缺表时不清空全局配置表

网页端全面响应式适配（PC / 平板 / 手机）

- 登录页大按钮修复（一处 CSS 类名撞车曾把按钮压塌）
- 日志/归档/可信设备等宽表格在窄屏改为卡内横向滚动，操作按钮不再被裁切
- 手机端触控目标放大、刘海/圆角屏安全区避让

> 详细变更与部署步骤见上文各功能章节。旧版本说明见下方 v3.2.0 及更早条目。

### v3.2.0（2026-10-04）

新增网页端功能：**对话预演**。升级只需替换二进制并重启，数据库结构无变化。

- 联系人详情页新增 **预演** 子页。描述一个要跟对方开口的场景（谈涨薪、拒绝借钱、表白……），AI 按这个人的画像与真实聊天语气扮演他跟你对练，可选「我先说」或「对方先说」
- 每条回应都带一个情绪标签，让你看到自己这句话把对方推到了什么状态；模型被明确要求不要为了配合你而软化立场
- 随时点 **结束并复盘**，AI 以沟通顾问身份退出角色，给出做得好 / 有问题 / 真去谈可能踩的坑 / 可以这么说，以及 0~10 分的达成可能评分；整场对话可一键复制
- 扮演依据来自画像（性格、沟通风格、口头禅、最近的事、雷区等 17 项）加上最近 30 条真实消息里挑出的说话样例，会在页面上原样展示出来供你核对
- **完全无状态**：预演过程不写数据库、不发消息，刷新页面或切换联系人即清空；后端对请求体做了 512KB、单条 2000 字、最多 60 轮的上限约束
- 新增接口 `GET /api/contacts/{id}/rehearsal/context`、`POST /api/contacts/{id}/rehearsal/turn`、`POST /api/contacts/{id}/rehearsal/review`（均要求已认证且配置了 LLM）

### v3.1.1（2026-10-04）

维护版本，不含新功能，集中修复 v2.4 以来累积的缺陷。升级只需替换二进制并重启，数据库自动迁移（`user_version` 升到 7，兼容旧库）。

**修复：数据一致性**

- 撤销合并联系人时，被合并方的**标签、日历事件、待跟进事项**之前不会一起回滚，导致撤销后留下孤儿数据；现在合并前把这三类记录快照进 `merge_log.undo_extra`，撤销时原样恢复
- 归档消息现在保留原始 `id`，恢复（`/api/archive/restore`）后与归档前完全一致，不再出现 id 漂移
- 归档调度器支持随进程退出而停止，并加了 `recover`，单次归档 panic 不再拖垮整个后台循环

**修复：时区与排序**

- 周报统计里按 SQL `CURRENT_TIMESTAMP` 写入的时间没有加 `'localtime'` 换算，跨时区部署时统计区间会偏移
- 归档判定和消息排序统一改用 `strftime('%s', msg_time)` 比较。`messages.msg_time` 是带时区偏移的 RFC3339 字符串，直接按字符串比较在混合偏移的库里会排错
- 社交活跃度面板的日期现在按时间先后输出，不再依赖 map 遍历顺序

**修复：安全**

- 邮件发送加了 SMTP 超时（拨号 / 握手 / 数据阶段），发件服务器无响应时不再无限期阻塞提醒任务
- 邮件头字段做 CRLF 过滤，主题和收件人里的换行不能再注入额外邮件头
- 日历订阅链接的 key 改用定长比较（`subtle.ConstantTimeCompare`）
- 可信设备令牌文件改为临时文件 + `rename` 原子写入，写入中途崩溃不再留下半截 JSON；解析失败的文件重命名为 `.corrupt` 保留现场，而不是直接丢弃全部可信设备
- 归档接口收到非法请求体时返回 400 并说明原因，不再当成「使用默认配置」静默执行

**修复：网页端**

- 看板加载时若某个统计接口返回空，整页会白屏（读 `undefined.length`）；现已加守卫，单块数据缺失只影响那一块
- 退出登录后改为整页刷新，避免上一个账号的联系人详情、草稿等状态残留到下一次登录
- 切换联系人时补齐弹窗和忙碌态的重置，之前快速连点两个联系人会看到上一个人的弹窗内容
- 联系人详情、聊天历史、统计、标签、归档等加载函数加了请求序号，快速切换时慢返回的旧请求不会再覆盖新数据
- 关系助手和归档设置里的数字输入框：清空后保存会送出空串，后端只回一句笼统的「请求体解析失败」；现在发请求前就拦下，提示具体是哪个字段、合法范围是多少，且上下界与后端校验一致
- 「立即归档」的确认框显示的是输入框里的天数，但实际发的是空请求体、后端按**已保存**的配置执行，两者可能不一致；现在把确认的天数一起提交
- 会话过期后停止后台轮询，不再反复弹出登录失效提示；只有 401/403 才清除本地令牌，网络故障不会把用户登出
- 之前静默失败的几处请求改为可见提示，归档和标签加载失败后页面上直接给出重试按钮
- 保存联系人画像成功后的收尾刷新若失败，错误信息会写进已经关闭的弹窗并残留到下次打开；现在刷新失败不再污染错误位
- 样式表补齐 `.hint`、`.hint.block`、`.list-head` 三条一直在被引用却没有定义的规则（提示文字此前和正文一样大、联系人页与合并记录页的标题行按钮会掉到下一行），并修正 6 处引用了不存在的 CSS 变量导致的边框丢失
- 登录页的「幽灵按钮」样式此前没有作用域前缀，`width:100%` 泄漏到全站所有 `.btn.ghost`；已限定在登录卡片内

### v3.1.0（2026-10-03）

**新增：待跟进事项（承诺 / 借钱 / 待回复）**

- 网页端「关系助手」页新增待跟进卡片，可手动记一笔（类型分承诺、金钱往来、待回复），带金额、截止说明
- 也可让 AI 从最近聊天里扫出「我答应过的事 / 借钱还钱 / 该回复没回复」，自动抽取默认关闭（有模型开销），开启后每天最多扫 8 个联系人、只看最近 30 天消息（均可配）
- 未完成项每天随提醒邮件一起推送，网页端可标记完成 / 忽略 / 重新打开，也可直接删除

**新增：联系人标签分组**

- 自定义标签（可改名、可删除），联系人详情页可勾选标签，列表页显示标签 chips 并支持按标签筛选
- 列表页支持勾选多人批量打标 / 批量去标（单次上限 2000 人）

**新增：聊天记录全文搜索**

- 网页端新增「洞察」页，第一个子面板就是全文搜索：跨全部联系人按关键词搜消息，可限定联系人、起止日期
- 可选一并搜已归档消息（默认只搜活动消息），支持翻页加载更多

**新增：联系人时间线**

- 联系人详情页新增「时间线」子页，按时间倒序汇总首次联系、画像更新、合并、手动记录的大事
- 可手动补记事件（标题 + 详情 + 时间），可删除；派生节点自动生成，不需维护

**新增：重要日子日历订阅（.ics）**

- 「关系助手 → 日历订阅」一键生成独立密钥，得到一条 `.ics` 订阅地址，手机/电脑日历客户端直接订阅
- 联系人的生日、纪念日按 RFC5545 导出为全天、每年重复的事件；订阅地址不走 Bearer，靠 URL 密钥鉴权（仍受 IP 白名单约束），可随时重置或关闭
- 密钥只显示一次（后续查看是打码值），泄露后重置即可

**新增：重要日子 AI 祝福语草稿**

- 重要日子临近时按联系人画像和你的说话风格生成 3 条草稿，网页端一键复制，**不代发**
- 提醒邮件里是否附草稿由开关控制，默认关闭

**新增：疑似重复联系人推荐**

- 「洞察」页按昵称/备注相似度扫出可能是同一个人的联系人对，给出建议保留项和判定理由，一键跳去合并页

**新增：年度关系报告**

- 选年份一键生成：消息量、最活跃的一天、月度走势、情绪曲线、亲密度变化、高频关键词、大事记
- 可导出为独立 HTML 长页另存分享（内联样式，不依赖服务端）

**新增：我的社交大盘**

- 全量联系人的互动统计：24 小时活跃分布、周几分布、双方平均回复速度、谁先开口的比例、你的高频用词
- 统计窗口可选（默认 30 天，最长 3650 天），纯本地计算、不调用模型

**修复**

- 修复备注为空（NULL）的联系人被静默跳过的问题：会导致生日提醒邮件和日历订阅对绝大多数联系人生效不了
- 修复年度关系报告在无数据时把情绪曲线、亲密度变化、大事记返回成 `null`（前端读不到 `.length`）的问题，统一返回 `[]`

**升级说明**

- 全部为网页端增值功能，默认不改变现有行为：AI 抽取待跟进、AI 祝福语草稿默认关闭，日历订阅不生成密钥就不生效；`config.json` 格式不变，直接替换二进制/镜像升级即可
- 新增的 4 张表（`contact_tags`、`contact_tag_links`、`contact_events`、`followup_items`）在启动时按需创建，随主库一起备份；即使这些表缺失，核心记录/查询/提醒功能也照常工作
- 升级后请强制刷新一次网页（Ctrl/Cmd + Shift + R）

### v3.0.0（2026-10-03）

**新增：消息归档（默认关闭，对现有功能零侵入）**

- 把超过保留期（默认 730 天 ≈ 2 年，可配 30 ~ 36500 天）的旧消息从 `messages` 表移入同库的 `messages_archive` 归档表，缩小活动数据、加快日常查询
- 网页端「备份」页新增「消息归档」卡片：开关每日自动归档（每 24 小时跑一次）、设置保留天数、立即归档、查看活动/已归档/可归档统计与按联系人明细
- 随时可恢复：按单个联系人或一键恢复全部；联系人已删除的归档消息不会写回（避免悬空），原地保留并单独标注
- 安全性：归档/恢复都在单事务里先复制、确认入档后再删除，`msg_hash` 撞车也不丢消息；`msg_time` 为空或非法的历史数据原地保留，宁少归不错归
- 归档表与主库同一个 SQLite 文件，备份（VACUUM INTO）天然覆盖，恢复时随表一起搬回

**新增：可信设备（免重复登录）**

- 登录页新增「信任此设备」勾选（默认勾选）：完整通过 Token + 2FA 登录后签发一枚 90 天有效的长效令牌存在浏览器，之后打开网页免输 Token 和动态验证码，每次使用自动滑动续期
- 网页端「备份」页新增「可信设备」卡片：查看全部可信设备（名称、打码令牌、添加/最近使用/到期时间、最近 IP）、单独吊销、一键吊销全部
- 安全边界：免登录换取会话仍受 IP 白名单和封禁名单约束；关闭 2FA 时自动吊销全部可信设备；令牌文件 `trusted_clients.json` 权限 0600、不进备份；设备数上限 20 台，超出自动淘汰最久未使用的
- 浏览器端点「退出登录」只对本标签页生效（不会立刻又自动登进去），重新打开页面仍免登录；要彻底退出请在「可信设备」里吊销

**升级说明**

- 两项功能默认均不改变现有行为：归档默认不启用（不开启就只是多建两张空表），可信设备只在登录时勾选才生效；`config.json` 格式不变，直接替换二进制/镜像升级即可
- 升级后请强制刷新一次网页（Ctrl/Cmd + Shift + R）

### v2.4.0（2026-10-03）

**新增：关系助手（网页端增值功能，默认关闭，对现有功能零侵入）**

- **重要日子提醒**：自动扫描联系人画像里的重要日子（生日、纪念日），支持「5月1日」「1995-05-01」「12/25」等常见写法，提前 N 天（可配）进入提醒；无法识别的原文（如农历）单独列在「未识别」区，不会误报
- **久未联系提醒**：超过 N 天（默认 7 天）无任何互动的联系人进入冷却名单，附带最后一条消息方便找话题；从没聊过的不参与
- **亲密度评分**：纯本地统计近 30 天互动（活跃度 40 分 + 双方均衡度 30 分 + 对方投入 30 分），Top10 看板展示，不调用模型、零成本
- **AI 情绪预警**：对近 3 天有消息的联系人做情绪分析（复用已配置的 LLM），情绪低落等给出摘要与建议；每日分析数量可配（默认上限 10 个）控制花费
- **每日提醒邮件**：每天固定时间（默认 08:00）把以上三类聚合成一封 HTML 邮件发到你的邮箱；无提醒事项时不发；同一事项窗口期内不重复提醒，发送失败下次自动补发
- **每周报告邮件**：每周固定时间（默认周日 20:00）发送本周消息量、互动最多、亲密度 Top5、最久没联系、下周重要日子、情绪速览
- **SMTP 配置**：网页端「关系助手」页填写邮箱 SMTP（465/SSL 或 587/STARTTLS 均支持），可发测试邮件验证；密码保存后打码显示；任务运行记录与邮件发送日志可查
- **手动运行**：网页端可立即触发每日提醒 / 每周报告，不用等定时
- 实现上全部逻辑在新文件（`assistant.go` / `assistant_api.go` / `mailer.go`），配置存 SQLite 新表，不改 `config.json` 格式；不开启时定时任务只读一行配置即返回，无任何副作用

### v2.3.3（2026-10-03）

**修复**

- **手机端网页顶栏菜单显示不全**：小屏顶栏改为两行布局——第一行「品牌 + 退出」，第二行导航独占整行、放不下时可横向滑动，联系人 / 合并记录 / 备份 / 状态 / 命令说明五个入口全部可达；此前单行布局里导航被品牌和退出按钮挤成一条缝，只能看到前一两个入口且不易发现可以滑动

### v2.3.2（2026-10-03）

**新增**

- **Docker 镜像随 Release 发布**：Release 附带 `wechat-profile-bot-docker-v2.3.2.tar.gz`，`docker load` 即可导入 `wechat-profile-bot:v2.3.2` 镜像，不再必须 git clone 源码构建；发布包内也新增 `Dockerfile` / `docker-compose.yml` / `docker-entrypoint.sh`，可自行构建

**变更**

- **完全移除 2FA 防重放限制**：不再记录已使用的时间步（`totp_secret.json` 的 `LastUsedStep` 字段废弃），同一验证码在其 30 秒有效窗口（含前后各 1 步容差）内可重复使用；此前多设备同时登录或重复提交会被误拒为「验证码已被使用」，现已不会。登录仍需密码 + 2FA 验证码两要素，安全性不受影响

**测试**

- 新增 TOTP 验证单测（正常步、窗口边界、错误码）；删除联系人回归测试同步更新

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
