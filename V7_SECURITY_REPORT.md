# V7 Security Report — 安全测试（蓝图 §27 十攻击面）

> 生成时间：2026-10-06（Phase 16 Documentation）
> 代码：`security_v7_test.go`（§27 十攻击面回归，9 测试）+ 既有 `security_test.go`（guard/limiter/headers，8 测试）
> 原则：**针对真实存在的防线写可执行断言**，不空喊口号；每项都能被攻击者绕过时立刻红。

---

## 1. 攻击面 ↔ 防线 ↔ 测试 总览（§27 十项全覆盖）

| # | 攻击面 | 真实防线（源码） | 钉住的测试 |
|---|--------|------------------|-----------|
| 1 | **SQL Injection** | 全仓参数化查询（`?` 绑定），无任何用户输入拼进 SQL | `TestSecuritySQLInjectionTreatedAsLiteral` |
| 2 | **FTS Query Injection** | `ftsPhraseMatch`（`fts.go:119`）把每关键词裹成带引号短语、内部双引号翻倍转义 | `TestSecurityFTSQueryIsNeutralized` |
| 3 | **SSRF** | `validateLLMBaseURL`（`llm_settings.go:281`）协议/凭据护栏 | `TestSecurityURLSchemeAllowlist` |
| 4 | **API Key Leakage** | 带口令备份时旁路密钥文件加密为 `*.enc`（`backup.go` sidecar） | `TestSecurityBackupSecretsEncryptedWithPassword` |
| 5 | **Authorization** | 设 `apiToken` 后校验 Bearer / 网页会话，否则 401（`api.go:281`） | `TestSecurityAuthorizationRequired` |
| 6 | **Rate Limit** | `rateLimiter` 固定窗口，`/api/ingest` 超限 429 + `Retry-After`（`api.go:1083`） | `TestSecurityIngestRateLimit` |
| 7 | **Path Traversal** | `extractBackupZip`（`backup.go:474`）拒 `name!=Base` / `.` / `..` / 含 `/\` 的目录条目 | `TestSecurityBackupRejectsTraversalEntries` |
| 8 | **Backup Secret Leakage** | 同 #4 加密 + 条目数上限 `backupMaxEntries` + 解压体积上限 | `TestSecurityBackupSecretsEncryptedWithPassword` · `TestSecurityBackupEnforcesEntryLimit` |
| 9 | **CORS** | `securityHeaders` **从不下发** `Access-Control-Allow-*`（默认同源即最强姿态） | `TestSecurityNoPermissiveCORS` |
| 10 | **Proxy Abuse** | `validateLLMProxyURL`（`llm_settings.go:301`）仅放行 http/https/socks5 | `TestSecurityURLSchemeAllowlist` |

---

## 2. 关键防线参数

| 项 | 值 | 出处 |
|----|----|------|
| 登录失败封禁阈值 | `maxAuthFailures = 10` → IP **永久**封禁 | `security.go:41` |
| 封禁持久化文件 | `banned_ips.json`，权限 **0600** | `security.go`（`TestSecurityGuard_BanFilePermissions` 钉） |
| ingest 限流 | `ingestRateLimit = 120 / 分钟`，按 token（无则按 IP） | `security.go:47` |
| 备份 zip 条目上限 | `backupMaxEntries = 20` | `backup.go` |
| 备份解压体积上限 | `backupMaxUnzipBytes = 512 MB`（防 zip bomb） | `backup.go` |
| 安全响应头 | `X-Content-Type-Options: nosniff` · `X-Frame-Options: DENY` · `Referrer-Policy` · CSP | `securityHeaders` |
| 慢速攻击防护 | `ReadHeaderTimeout = 10s`（防 slowloris）·`IdleTimeout=120s` | `api.go` |

---

## 3. 逐攻击面断言要点

- **SQLi**：四类载荷（`'); DROP TABLE…` / `" OR "1"="1` / `1; DELETE…` / `' UNION SELECT…--`）作为
  联系人姓名与消息内容写入 → 断言 `messages/contacts/ai_response_cache/profile_facts` 表**仍在**（schema 未被破坏）、
  内容**逐字**回读一致、以注入串作搜索词**不报错也不命中全部**（证明是字面量而非语法）。
- **FTS 注入**：逐条断言转义产物——`a" OR "1"="1 → "a"" OR ""1""=""1"`（整体一个短语，`OR` 失效）、
  `NEAR(...)` / `*` / `col:value` 全部被引号裹成字面短语；多词以 `AND` 连接、无一泄漏裸语法。
- **SSRF/Proxy**：代理放行 `""`/http/https/socks5，拒 `ftp/file/gopher/dict/://x`；Base URL 拒带 `userinfo` 凭据与 `file://`。
  **诚实边界**：`validateLLMBaseURL` 有意放行 `localhost`（支持本地模型服务器如 ollama），故本测试只锁**协议白名单 + 凭据拒绝**，不谎称拦所有内网地址。
- **备份密钥加密**：向 sidecar `config.json` 植入哨兵 API Key → 带口令备份产出 `config.json.enc`、**不含**明文 `config.json`、
  扫描全文明文**不含哨兵**；对照组（无口令）含明文，证明断言非恒真。
- **认证**：`/api/status` 无凭证 / 错 Bearer → 401；正确 `Bearer secTok` → 非 401。
- **限流**：`ingestRL` 上限=2，第 1/2 次放行（空 text 早退 400），第 3 次 → **429 + Retry-After**。
- **穿越/zip bomb**：`../`、`sub/`、`a\b`、`..` 条目 → extract 报「非法路径」；`backupMaxEntries+1` 条目 → 报「文件数」超限。
- **CORS**：经 `securityHeaders` 的响应**不含**任何 `Access-Control-Allow-Origin/Credentials`，同时 `nosniff`/`DENY` 在位。

---

## 4. 运行结果（全绿）

```
go test -run TestSecurity ./...   →  ok   wechat-profile-bot   0.209s
```
9 项 §27 攻击面测试 + 8 项既有 guard/limiter/headers 测试全部 PASS。

---

## 5. 依赖面最小化（供应链安全）

`go.mod` 直接依赖仅三项，无高危/网络监听面：
`glebarez/go-sqlite`（纯 Go SQLite）· `go-resty/resty/v2`（出站 HTTP 客户端）· `skip2/go-qrcode`（TOTP 二维码）。
§32 违禁栈（Redis / Elasticsearch / 向量库 / Hook / UI Automation）源码 **0 命中**。

---

## 6. 遗留与建议（诚实清单，非阻断项）

- `banned_ips` 为**永久**封禁，无误封自动过期——已提供 `--unban <ip>` / `--list-bans` 运维通道；如需自动解封可后续加 TTL（本版本按设计保持永久）。
- 认证为 Bearer Token + 网页 TOTP 双因素；桌面端远程模式直用 apiToken——建议生产环境配 `APIWhitelist`/`TrustedProxies` 叠加 IP 维度（能力已具备）。
- 本报告覆盖的是**回归可自动化的攻击面**；密钥静态存储加密、TLS 终结等属部署层，不在应用代码范围内。
