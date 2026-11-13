# Qoder 2API 技术架构与配置手册

本文档提供 **Qoder 2API** 网关（默认监听 `:7865`）的完整系统架构设计、配置全集、环境变量映射、双域路由逻辑、API 接口规范与故障排查速查表。

---

## 目录

1. [系统整体架构](#1-系统整体架构)
2. [配置文件详述 (`config.json`)](#2-配置文件详述-configjson)
3. [环境变量覆盖清单](#3-环境变量覆盖清单)
4. [双域路由与模型映射机制](#4-双域路由与模型映射机制)
5. [HTTP 协议接口规范](#5-http-协议接口规范)
   - [AI 核心协议端点](#51-ai-核心协议端点)
   - [网关监控与系统运维端点](#52-网关监控与系统运维端点)
   - [Web 控制台与认证端点](#53-web-控制台与认证端点)
6. [凭证解析机制与账号池加权调度](#6-凭证解析机制与账号池加权调度)
7. [全自动北京时间 (CST) 调度引擎](#7-全自动北京时间-cst-调度引擎)
8. [网络安全、IP 白名单与多租户 RBAC](#8-网络安全ip-白名单与多租户-rbac)
9. [CLI 命令行工具手册](#9-cli-命令行工具手册)
10. [异常代码字典与故障排查](#10-异常代码字典与故障排查)

---

## 1. 系统整体架构

Qoder 2API 采用纯 Go 语言编写，专为企业级高可用模型转发打造：

```text
                               +---------------------------------------------------+
                               |                   Client Apps                     |
                               |  (NextChat / Cherry Studio / Claude Code / SDK)   |
                               +-------------------------+-------------------------+
                                                         |
                                                         | HTTP / SSE Stream
                                                         v
+----------------------------------------------------------------------------------------------------------+
|                                           Qoder 2API Gateway (:7865)                                     |
|                                                                                                          |
|   +--------------------------+   +--------------------------+   +------------------------------------+   |
|   |   IP Whitelist Guard     |   |   Master & User API Key  |   |        Anti-Bruteforce Lock        |   |
|   +--------------------------+   +--------------------------+   +------------------------------------+   |
|                                                                                                          |
|   +------------------------------------+   +---------------------------------------------------------+   |
|   |         Protocol Adapters          |   |                 Web Console & Portal                    |   |
|   |  - OpenAI: /v1/chat/completions    |   |  - Cyber Glass Console (/console)                       |   |
|   |  - Anthropic: /v1/messages         |   |  - Public Service Status (/)                            |   |
|   |  - OpenAI Codex: /v1/responses     |   |  - PKCE OAuth / Local Scanner / Multi-Tenant RBAC       |   |
|   +------------------+-----------------+   +---------------------------------------------------------+   |
|                      |                                                                                   |
|                      v                                                                                   |
|   +--------------------------------------------------------------------------------------------------+   |
|   |                                  Smart Account Pool Scheduler                                    |   |
|   |  - 3-Factor Weight (Quota + 7D Expiring + Idle)                                                  |   |
|   |  - Dual-Realm Isolation & Fallback (CN & Global & VPC)                                           |   |
|   |  - Rate-Limit Backoff (429/110) & 10605 Queue Ping & Circuit Breaker                             |   |
|   |  - Sticky Session Routing (Conversation Continuity)                                              |   |
|   +------------------+-------------------------------------------------------------------------------+   |
|                      |                                                                                   |
|                      v                                                                                   |
|   +--------------------------------------------------------------------------------------------------+   |
|   |                                    Upstream COSY Protocol Client                                 |   |
|   |  - Official COSY Packet Signing & Header Masquerading                                            |   |
|   |  - Multi-Gateway Failover (api1.qoder.sh / api2.qoder.sh / api3.qoder.sh / gateway.qoder.com.cn) |   |
|   |  - CST UTC+8 Automation (Daily Campaigns Check-in, Pro Benefits, Token Keepalive)               |   |
|   +--------------------------------------------------------------------------------------------------+   |
+----------------------------------------------------------------------------------------------------------+
                               |                                           |
                               v                                           v
               +-------------------------------+           +-------------------------------+
               |    Qoder CN (国内版 & VPC)    |           |       Qoder Global (国际版)   |
               |  - https://gateway.qoder.com.cn |         |  - https://api3.qoder.sh      |
               |  - 通义千问全系 / 国内免费节点  |           |  - Claude 3.7 Sonnet / Opus   |
               +-------------------------------+           +-------------------------------+
```

---

## 2. 配置文件详述 (`config.json`)

配置文件位于运行根目录下的 `config.json`（初次启动若不存在将按 `config.example.json` 自动生成）。

### 2.1 顶层核心配置

| 配置键名 | 类型 | 默认值 | 详细说明 |
| --- | --- | --- | --- |
| `listen` | string | `":7865"` | 本地 HTTP 服务绑定的端口与地址。 |
| `api_key` | string | `"sk-qoder2api"` | 主 API 访问密钥。用于认证客户端请求的 Bearer Token 或 `x-api-key`。 |
| `web_password` | string | `""` | 控制台管理员登录密码。留空时允许从本机 `127.0.0.1` 首次免密初始化。 |
| `ip_whitelist_enabled`| bool | `false` | 是否开启 API 接口 IP 白名单防护。 |
| `allowed_ips` | []string | `[]` | 允许访问的客户端 IP 或 CIDR 网段（如 `["192.168.1.0/24", "10.0.0.5"]`）。 |
| `allow_localhost` | bool | `true` | 当白名单开启时，是否始终放行本机回环地址（`127.0.0.1` 与 `::1`）。 |
| `allow_remote_admin` | bool | `false` | 是否允许非 localhost 外部 IP 访问 `/console` 后台。 |
| `auth_dir` | string | `"./auths"` | 凭证热加载目录。系统每 5 秒自动检测新增或变动的账号文件。 |
| `state_file` | string | `"./data/state.json"`| 账号池运行状态持久化快照路径。同级目录保存用户中心与运行时设置。 |

### 2.2 `global`（双域与端点）配置

```json
{
  "global": {
    "enabled": true,
    "api_base_cn": "",
    "api_base_global": "",
    "openapi_cn": "",
    "openapi_global": "",
    "website_cn": "",
    "website_global": "",
    "vpc": ""
  }
}
```
- `enabled`: 是否启用国际版（Global）路由。设为 `false` 时所有请求将被强制约束在国内版。
- `vpc`: 国内版企业 VPC 统一实例 Host（例如 `company.vpc.qoder.com.cn`）。
- `api_base_*` / `openapi_*` / `website_*`: 上游官方域名覆写项，专用于内网反向代理或专线加固环境。

### 2.3 `pool`（账号池加权与容灾）配置

```json
{
  "pool": {
    "max_in_flight": 3,
    "max_in_flight_global": 2,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "breaker_cooldown_max": "6h",
    "degrade_threshold": 5,
    "degrade_cooldown": "10m",
    "degrade_cooldown_max": "2h",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0,
    "expiring_soon": "168h",
    "cost_explore_interval": "30m",
    "max_rotate": 8
  }
}
```
- `max_in_flight` / `max_in_flight_global`: 单个国内/国际版账号最大允许的并发请求数（防超限封控）。
- `expiring_soon`: 临期额度优先消耗时间窗口（默认 `168h` = 7 天），即将过期的积分将在调度时赋予最高权重。
- `breaker_threshold` / `breaker_cooldown`: 连续硬性失败熔断阈值及初始冷却时间。
- `degrade_threshold` / `degrade_cooldown`: 降级阈值与恢复窗口。

### 2.4 `schedule`（自动化排程）配置

```json
{
  "schedule": {
    "checkin_enabled": true,
    "checkin_hours": [10, 22],
    "keepalive_enabled": true,
    "keepalive_hours": [4, 16],
    "pro_benefit_enabled": true,
    "pro_benefit_hours": [11]
  }
}
```
- 全部排程均基于北京时间（CST UTC+8）执行。
- `checkin_hours`: 每天自动全池签到收割官方活动的时间点。
- `pro_benefit_hours`: 每天自动检测并申领 Pro 升级试用权益的时间点。
- `keepalive_hours`: 每天全池刷新 Token 避免过期的预热时间点。

---

## 3. 环境变量覆盖清单

所有通过环境变量设置的项优先级高于 `config.json`，在容器化或云端部署时尤为便捷：

| 环境变量 | 作用说明 | 示例值 |
| --- | --- | --- |
| `QD2A_LISTEN` | 覆盖服务端口与地址 | `":7865"` |
| `QD2A_API_KEY` | 覆盖主 API Key | `"sk-my-super-secret-key"` |
| `QD2A_WEB_PASSWORD` | 覆盖控制台密码 | `"admin888"` |
| `QD2A_ALLOWED_IPS` | 逗号分隔的 IP/CIDR 列表（自动激活白名单） | `"127.0.0.1,192.168.1.0/24"` |
| `QD2A_ALLOW_LOCALHOST` | 是否允许回环地址访问 | `"true"` / `"false"` |
| `QD2A_GLOBAL_ENABLED` | 是否激活国际版能力 | `"true"` / `"false"` |
| `QD2A_GLOBAL_VPC` | 全局 CN VPC 接入点 | `"vpc-prod.qoder.com.cn"` |
| `QD2A_PROXY` / `QD2A_UPSTREAM_PROXY` | 上游 HTTP/SOCKS5 代理 | `"socks5://127.0.0.1:10808"` |
| `QD2A_EXPIRING_SOON` | 临期积分优先消耗窗口 | `"168h"` |
| `QD2A_UPSTASH_URL` | Upstash Redis REST URL (可选集群状态同步) | `"https://...upstash.io"` |
| `QD2A_UPSTASH_TOKEN` | Upstash Redis REST Token | `"AXxxxxxx..."` |

---

## 4. 双域路由与模型映射机制

### 4.1 域前缀语法与自动分流

客户端请求的模型支持使用前缀显式指定所属域：
- `cn:qwen3.8-flash` 或 `cn/qwen3.8-flash`：强制路由至国内版（CN）。
- `global:smodel`、`intl:smodel` 或 `global/smodel`：强制路由至国际版（Global）。
- **无前缀请求**：
  1. 独占模型自动识别：如 `sonus`、`smodel`、`cantus`、`ultimate` 自动匹配至国际版；`q37fmodel`、`gm51model` 自动匹配至国内版。
  2. 共享模型（如 `qwen3.8-flash`）：优先按账号池健康度、余额充沛度与免费节点策略进行最优分流。

### 4.2 常用模型别名字典 (`ModelAliases`)

| 客户端请求名称 | 内部映射 Key | 默认推荐域 | 背后对标模型 |
| --- | --- | --- | --- |
| `claude-3-7-sonnet` / `claude-sonnet-4` | `smodel` | 国际版 | Anthropic Claude 3.7 Sonnet |
| `claude-opus-4` / `cantus` | `cmodel` | 国际版 | Anthropic Claude Opus |
| `qwen3.8-flash` | `qfmodel` | 国内版 / 国际版 | 通义千问 3.8 Flash（多模态免费） |
| `qwen3.8-max` | `qmodel_38max` | 国际版 | 通义千问 3.8 Max |
| `deepseek-v4-pro` | `dmodel` | 国际版 | 深度求索 DeepSeek V4 Pro |
| `deepseek-flash` | `dfmodel` | 国际版 | DeepSeek Flash |
| `glm-5.3` | `gmodel` | 国际版 | 智谱 GLM-5.3 |
| `kimi-k3` | `kmodel_latest` | 国际版 | 月之暗面 Kimi K3 |
| `minimax-m3` | `mmodel` | 国际版 | MiniMax M3 |
| `qwen3.7-flash` | `q37fmodel` | 国内版 | 通义千问 3.7 Flash |
| `glm-5.2` | `gm51model` | 国内版 | 智谱 GLM-5.2 |

---

## 5. HTTP 协议接口规范

### 5.1 AI 核心协议端点

客户端发起调用时需在请求头携带鉴权信息：
- `Authorization: Bearer <api_key>` 或 `x-api-key: <api_key>`

#### 1. 模型列表 `GET /v1/models`
- 返回聚合了国内版与国际版当前可用模型的 OpenAI 格式列表。

#### 2. 对话补全 `POST /v1/chat/completions`
- **支持格式**：JSON 请求体（标准 OpenAI 格式）。
- **特性支持**：
  - `stream: true`：标准 SSE 流式传输，自动对齐 Thinking 思维链及 Usage 统计。
  - `tools` / `tool_choice`：原生支持 Function Calling。
  - 多模态图文输入：自动将 `image_url` 下载并转换为上游所需结构。

#### 3. Anthropic 消息接口 `POST /v1/messages`
- 原生兼容 Anthropic Claude 协议及 Claude Code CLI。
- 支持 `POST /v1/messages/count_tokens` Token 估算。

#### 4. OpenAI Codex 接口 `POST /v1/responses`
- 专为代码补全及 Agent responses 设计，支持命名空间工具展开。

---

### 5.2 网关监控与系统运维端点

- `GET /healthz`：极速存活探针。账号池有可用账号时返回 `200 OK`，否则返回 `503`。
- `GET /status`：获取账号池总数、各域可用分布、健康详情（需鉴权）。
- `GET /v1/stats`：查询累计 Token 统计、TPS、TTFB 响应延迟分布与调用量。
- `POST /admin/accounts/{uid}/disable`：手动临时封禁指定账号。
- `POST /admin/accounts/{uid}/enable`：手动重新启用指定账号。
- `POST /admin/accounts/{uid}/revive`：一键清除账号冷却与熔断标记。

---

### 5.3 Web 控制台与认证端点

- `GET /console` 或 `GET /dashboard`：管理员控制台界面。
- `POST /ui/oauth/start`：发起 PKCE 设备流授权，返回浏览器登录 URL。
- `POST /ui/oauth/poll`：轮询设备流授权结果，用户在浏览器授权后自动落盘入库。
- `POST /ui/oauth/batch_import`：批量导入 JSON 凭据、卡密文本或 ZIP 备份。
- `GET /ui/local/scan`：扫描本机已安装的 Qoder Desktop 与 CLI 凭证。

---

## 6. 凭证解析机制与账号池加权调度

### 6.1 支持的账号凭证格式

Qoder 2API 支持多种灵活的录入方式：
1. **官方标准凭据文件 (`qoder-*.json`)**：
   包含 `uid`、`accessToken`、`refreshToken`、`expiresAt`、`realm`（`cn` 或 `global`）。
2. **个人访问令牌 PAT (`pt-...`)**：
   网关自动向上游发起换票，兑换为运行态的 `accessToken`。
3. **卡密键值对 (`token----refreshToken`)**：
   直接在批量导入窗口以文本形式粘贴。

### 6.2 三因子加权调度算法

每次收到请求时，网关执行以下选取流程：
1. **域过滤**：根据模型显式前缀或排他特征选定目标域候选池。
2. **硬性指标过滤**：排除正在熔断、软限频冷却中、并发满载（`in_flight >= max_in_flight`）的账号。
3. **动态加权评分**：
   $$\text{Score} = \text{CreditsWeight} + \text{ExpiringWeight} + \text{IdleBonus}$$
   - **`ExpiringWeight`（临期高优先）**：若账号在 7 天内存在到期积分，按临期比例极大提升权重，确保优先消耗。
   - **`IdleBonus`（空闲平滑）**：账号空闲时间越长得分越高，避免单账号持续高频调用。
4. **会话粘性保障**：若开启 `session_sticky`，相同会话 ID 在 TTL 内优先路由至同一账号以利用上游 KV 缓存。

---

## 7. 全自动北京时间 (CST) 调度引擎

系统内部运行了与北京时间（CST UTC+8）严格对齐的高精度定时排程器：

| 任务标识 | 默认触发时间 (CST) | 核心行为说明 |
| --- | --- | --- |
| `checkin` | 10:00, 22:00 | 遍历账号池全量账号，拉取 `/sash/api/v1/me/campaigns` 活动列表并自动领取积分奖励；遇到 401 自动触发 Token 刷新并重试。 |
| `pro_benefit` | 11:00 | 探测官方 Pro 会员升级资格与试用权益，自动提交申领。 |
| `keepalive` | 04:00, 16:00 | 在业务低峰期静默刷新即将到期的 RefreshToken，保障全天在线。 |

---

## 8. 网络安全、IP 白名单与多租户 RBAC

1. **真实来源 IP 校验**：
   - 杜绝随意伪造 `X-Forwarded-For`。仅当直接 TCP 连接来源为受信任的本机或私网代理节点时，才采信反向代理传递的客户端 IP。
2. **防爆破阶梯锁定机制**：
   - 同一客户端 IP 连续多次使用非法 API Key 或输错管理密码，网关将在内存中执行自动阶梯封锁（返回 `429 Too Many Requests`）。
3. **多租户 RBAC 隔离**：
   - 支持创建 `User` 角色，分配独立专属的 API Key，支持单独限制其最大 RPM 与每日消费积分额度。

---

## 9. CLI 命令行工具手册

```text
用法:
  qoder2api [serve] [flags]     启动网关服务 (Windows 默认启动原生 WebView2 GUI)
  qoder2api login [flags]       通过 PKCE OAuth 登录、导入 PAT 或扫描本机凭据
  qoder2api signin [flags]      手动执行全池日常官方活动签到
  qoder2api acct <action>       管理账号凭据 (list | scan | remove <uid>)
  qoder2api credit [flags]      查询全池实时算力余额与 7 日内临期额度表
  qoder2api models [flags]      列出 CN 与 Global 官方目录模型及乘数倍率
  qoder2api stats [flags]       查询运行中网关的 /v1/stats 使用指标
```

---

## 10. 异常代码字典与故障排查

### 1. `503 no_healthy_account`
- **现象**：客户端收到 `no healthy account available in pool`。
- **根因**：当前账号池中可用健康账号数为 0（可能尚未录入账号，或所有账号均处于限频/冷却状态）。
- **处理方案**：在控制台点击 **“🇨🇳 授权国内版账号”** 或 **“🔍 扫描本地凭据”** 载入账号。

### 2. `code: 112, pricingUrl: "https://qoder.com/pricing?client=qoder"`
- **现象**：调用模型时上游返回 112 错误并附带定价页链接。
- **根因**：当前调用使用了 **Qoder 国际版（Global）** 账号，但该账号在官方没有开通付费 Pro 订阅，可用余额为 `0.0`。
- **处理方案**：
  1. 推荐直接在控制台授权 **国内版（CN）** 账号（国内版个人用户自带免费额度）。
  2. 若需使用国际版，前往官方定价页开通订阅或申领新用户试用。

### 3. `500 Internal Server Error` (上游返回 `TOKEN_INVALID`)
- **现象**：调用时上游返回 HTTP 500 或 Token 无效提示。
- **根因**：误将 OAuth 设备授权网址（`https://qoder.com/device/selectAccounts?...`）当作 Token 导入到了系统中。
- **处理方案**：网关已内置 URL 防呆拦截；请在控制台直接使用一键 OAuth 授权流程，无需手动复制网址作为 Token。

### 4. `10605` (上游排队等待)
- **现象**：高并发期上游返回 `RESOURCE_LIMIT / 10605`。
- **网关行为**：系统内置排队重试控制器，期间每隔 3 秒自动向下游客户端发送 `: ping` SSE 心跳保持连接，直到上游生成首个 Token 或超时。
