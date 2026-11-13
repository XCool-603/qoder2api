<h1 align="center">Qoder 2API</h1>

<p align="center">
  <b>将 Qoder / 通义灵码（国内版 & 国际版）多账号汇聚为标准 OpenAI / Anthropic / Codex 兼容接口的高可用双域网关</b><br>
  单文件桌面端 · 双域额度与7日临期积分优先调度 · AI 接口 IP 白名单 · 免费模型直连 · PKCE 自动化授权与本地凭据扫描 · Docker 一键部署
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="Release" src="https://img.shields.io/badge/Release-v1.0.0-blue?style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI%20%7C%20Anthropic%20%7C%20Codex-412991?style=flat-square">
  <img alt="Platform" src="https://img.shields.io/badge/Platform-Windows%20%7C%20Linux%20%7C%20macOS-emerald?style=flat-square">
  <img alt="GUI" src="https://img.shields.io/badge/GUI-Native%20WebView2-teal?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single%20EXE%20%7C%20Docker-2496ED?logo=docker&logoColor=white&style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

<p align="center">
  👉 <b><a href="DOCUMENTATION.md">【点击查看：完整架构、配置与 API 技术手册 (DOCUMENTATION.md)】</a></b> 👈
</p>

---

## 🌟 核心能力

1. **双域原生支持 + 三协议兼容 + DeepSeek Harness (`dsh`) 内嵌**
   - **双域融合调度**：
     - **国际版（`global` / `qoder.com`）**：原生接入 Claude 3.7 Sonnet (`sonus` / `smodel`)、Claude Opus (`cantus` / `cmodel`)、Qwen 3.8/3.7 Max、DeepSeek V4 Pro (`dmodel`)、GLM 5.3 (`gmodel`)、Kimi K3 (`kmodel_latest`)、MiniMax M3、Auto / Ultimate / Performance 智能路由模型。
     - **国内版（`cn` / `qoder.com.cn` / 通义灵码）+ 企业专属 VPC（`*.vpc.qoder.com.cn`）**：支持国内官方低倍率及免费模型（如 `qwen3.8-flash`、`qwen3.7-flash`、`glm-5.2`、`minimax-m2.7` 等），支持显式前缀指定（如 `cn:qwen3.8-flash` 或 `global:smodel`），未指定前缀时根据模型独占特征与账号池健康度自动跨域路由。
   - **三协议全面兼容**：
     - **OpenAI 协议**：标准 `/v1/chat/completions`、`/v1/models`，完整支持 SSE 流式、Thinking 深度思维链、Function Calling 工具调用与多模态输入（图片 URL 自动转 Base64）。
     - **Anthropic 协议（兼容 Claude Code CLI）**：标准 `/v1/messages`、`/v1/messages/count_tokens`，原生无缝接入 Claude Code 命令行客户端。
     - **OpenAI Codex 协议**：标准 `/v1/responses`，支持命名空间工具扁平化（Namespace Tool Flattening）与推理块透传。
   - **DeepSeek Harness (`dsh`) 内嵌**：支持一键免配置集成便携 Node.js 运行时，直接在 Web 控制台标签页调度执行 Agent 任务。

2. **现代化赛博玻璃 Console 控制台与双域额度明细体系**
   - **前台门户 (`/`) + 管理控制台 (`/console` / `/dashboard`) 分离**：前台门户安全脱敏展示节点状态与模型矩阵；后台控制台一站式掌控运维。
   - **实时算力与临期积分感知**：实时查询 Qoder 官方 `/api/v2/quota/usage` 与 `/billing/meter` 接口，自动拆解 **总剩余额度 (`remain`)** 与 **7 日内临期额度 (`expiring_soon`)**，并在控制台顶部直观展现。
   - **全链路在线调试**：免外部客户端，直接在控制台内置的「智能网关全链路调试」标签页验证模型流式响应、延迟（TTFB）与路由状态。

3. **智能三因子加权调度 + 临期积分优先消耗 + 免费模型直连**
   - **三因子加权轮转**：综合 **剩余额度**、**7日临期额度**、**空闲时长衰减** 三大因子动态加权选取最优账号，最大限度保障临期积分不过期被浪费。
   - **免费模型零成本护盾**：针对 `Qwen3.8-Flash` 等零倍率免费模型直连调度，自动规避高成本扣费账号。
   - **智能限频与容灾熔断**：遇到上游 `10605`（排队等待）自动重试保活并下发 `: ping` SSE 心跳；遇到 `429` / `110`（软限频）智能阶梯退避；账号失效（`401`）自动重试刷新并剔除脏凭据。

4. **多途径极速上号 + 本地凭据智能扫描**
   - **一键 PKCE OAuth 2.0 登录**：控制台点击国内版或国际版授权，自动唤起官方授权页并生成二维码，扫码或网页确认后秒级自动轮询入库，无需手动抓包。
   - **本机凭据自动扫描**：内置跨平台凭据探测引擎，一键解密导入本地已安装的 **Qoder Desktop**（Windows DPAPI + AES-256-GCM 解密）与 **Qoder CLI**（`~/.qoder/.auth`、`~/.qoder-cn/.auth`）登录态。
   - **多格式批量导入与备份**：支持 JSON 凭据、个人访问令牌 PAT（`pt-...` 自动兑换为会话凭据）、多行卡密 TXT 以及 ZIP 全量打包备份与还原。

5. **全自动北京时间（CST）排程引擎**
   - **每日官方活动自动签到**：每天定时（CST 10:00 / 22:00）全池自动参与官方 `/sash/api/v1/me/campaigns` 活动，收割活动算力奖励。
   - **Pro 升级权益自动申领**：每天定时（CST 11:00）自动检测并申领官方 Pro 试用或升级权益。
   - **会话静默保活**：定时（CST 04:00 / 16:00）刷新即将过期的 RefreshToken，杜绝对话高峰期掉线。

6. **企业级安全防护 & IP 白名单**
   - **IP / CIDR 白名单过滤**：严格校验来源真实 IP，支持单个 IP 与网段过滤，非白名单请求直接阻断返回 403。
   - **控制台防爆破锁定**：同一 IP 连续输错密码或连续使用非法 API Key 触发自动阶梯锁定。
   - **多租户 RBAC 权限管理**：支持多用户独立 API Key、独立 RPM 频控与每日积分消费上限配额。

---

## 🚀 快速上手

### 方式 A：Windows 桌面客户端（最简单）

1. 从 [Releases 页面](https://github.com/XCool-603/qoder2api/releases) 下载最新版 `qoder2api.exe`；
2. 放入任意独立文件夹并双击运行：
   - 自动生成 `config.json` 与 `./data`、`./auths` 目录；
   - 自动弹出基于原生 Edge WebView2 的桌面管理窗口（无控制台黑框）；
3. 在弹出的控制台点击 **“🇨🇳 授权国内版账号”** 或 **“🌍 授权国际版账号”** 完成登录即可开始使用。

---

### 方式 B：Docker Compose 一键部署

```bash
git clone https://github.com/XCool-603/qoder2api.git
cd qoder2api
cp config.example.json config.json
docker compose up -d
```

- **网关端口**：`7865` (`http://127.0.0.1:7865`)
- **控制台地址**：`http://127.0.0.1:7865/console`
- **默认 API Key**：`sk-qoder2api`
- **持久化目录**：`./auths`（凭据）、`./data`（状态与配置）

常用维护命令：
```bash
# 查看实时日志
docker compose logs -f qoder2api

# 服务健康检查
curl -s http://127.0.0.1:7865/healthz

# 重启或停止服务
docker compose restart
docker compose down
```

---

### 方式 C：源码编译（Go 1.22+）

```bash
git clone https://github.com/XCool-603/qoder2api.git
cd qoder2api

# 1. 编译 Windows 纯桌面客户端（无黑框，原生 WebView2）
go build -ldflags="-s -w -H windowsgui" -o qoder2api.exe ./cmd/server

# 2. 编译 Linux / macOS / Windows 无头服务器端（适合在 VPS / 容器运行）
go build -ldflags="-s -w" -o qoder2api-headless ./cmd/server

# 3. 运行无头模式
./qoder2api-headless -headless -config config.json
```

---

### 方式 D：CLI 命令行运维

`qoder2api` 提供了丰富的开箱即用命令行工具：

```bash
# 启动无头服务
qoder2api serve -headless -config config.json

# 交互式 OAuth 登录（支持国际版 global / 国内版 cn）
qoder2api login -realm cn
qoder2api login -realm global

# 导入个人访问令牌（PAT，pt- 开头）并自动完成会话兑换
qoder2api login -realm cn -pat pt-xxxxxxxxxxxx

# 自动扫描并导入本机已登录的 Qoder Desktop 或 CLI 凭据
qoder2api login -scan

# 手动触发全池每日活动签到与权益申领
qoder2api signin

# 查看账号池当前所有账号、健康状态与所属域
qoder2api acct list

# 查询全池账号实时算力余额与 7 日内临期积分统计表
qoder2api credit

# 打印当前国内版与国际版支持的官方模型矩阵与计费倍率
qoder2api models

# 查看网关实时运行指标（请求量、TPS、Token 消耗等）
qoder2api stats
```

---

## ⚙️ 配置说明 (`config.json` 与环境变量)

首次启动会自动生成标准 `config.json`，核心配置结构如下：

```json
{
  "listen": ":7865",
  "api_key": "sk-qoder2api",
  "web_password": "",
  "ip_whitelist_enabled": false,
  "allowed_ips": [],
  "allow_localhost": true,
  "allow_remote_admin": false,
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "global": {
    "enabled": true,
    "vpc": ""
  },
  "pool": {
    "max_in_flight": 3,
    "max_in_flight_global": 2,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "degrade_threshold": 5,
    "degrade_cooldown": "10m",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0,
    "expiring_soon": "168h"
  },
  "schedule": {
    "checkin_enabled": true,
    "checkin_hours": [10, 22],
    "keepalive_enabled": true,
    "keepalive_hours": [4, 16],
    "pro_benefit_enabled": true,
    "pro_benefit_hours": [11]
  },
  "session_sticky": {
    "enabled": true,
    "ttl": "30m"
  }
}
```

### 常用环境变量覆盖清单

所有关键配置均支持环境变量注入，方便容器化部署：

| 环境变量 | 对应配置项 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `QD2A_LISTEN` | `listen` | `":7865"` | 服务监听地址 |
| `QD2A_API_KEY` | `api_key` | `"sk-qoder2api"` | 网关全局主 API Key |
| `QD2A_WEB_PASSWORD` | `web_password` | `""` | Web 控制台管理员密码（为空时仅本机免密） |
| `QD2A_IP_WHITELIST_ENABLED` | `ip_whitelist_enabled` | `false` | 是否开启 IP 白名单防御 |
| `QD2A_ALLOWED_IPS` | `allowed_ips` | `""` | 允许访问的 IP 或 CIDR（逗号分隔） |
| `QD2A_ALLOW_LOCALHOST` | `allow_localhost` | `true` | 白名单开启时是否始终放行回环地址 |
| `QD2A_GLOBAL_ENABLED` | `global.enabled` | `true` | 是否启用 Qoder 国际版路由 |
| `QD2A_GLOBAL_VPC` | `global.vpc` | `""` | 国内版企业私有 VPC 统一接入点 |
| `QD2A_PROXY` | `upstream.proxy` | `""` | 访问 Qoder 官方的上游 HTTP/SOCKS5 代理 |
| `QD2A_EXPIRING_SOON` | `pool.expiring_soon` | `"168h"` | 临期积分优先消耗窗口（默认 7 天） |

---

## 🔌 客户端与 API 接入示例

- **OpenAI 兼容 Base URL**：`http://127.0.0.1:7865/v1`
- **Anthropic 兼容 Base URL**：`http://127.0.0.1:7865`
- **全局接入密钥**：`sk-qoder2api`（或你在控制台生成的用户级 API Key）

### 1. cURL 命令行对话

```bash
curl -N http://127.0.0.1:7865/v1/chat/completions \
  -H "Authorization: Bearer sk-qoder2api" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen3.8-flash",
    "messages": [
      {"role": "user", "content": "你好，请用一句话介绍你自己！"}
    ],
    "stream": true
  }'
```

### 2. Python (`openai` SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:7865/v1",
    api_key="sk-qoder2api",
)

response = client.chat.completions.create(
    model="qwen3.8-flash",
    messages=[{"role": "user", "content": "请写一段 Python 快速排序代码"}],
    stream=True,
)

for chunk in response:
    content = chunk.choices[0].delta.content or ""
    print(content, end="", flush=True)
print()
```

### 3. Claude Code CLI 接入

Qoder 国际版原生支持 Claude 3.7 Sonnet 算力，可通过标准 Anthropic 接口无缝接入 Claude Code：

```bash
# Linux / macOS
export ANTHROPIC_BASE_URL="http://127.0.0.1:7865"
export ANTHROPIC_AUTH_TOKEN="sk-qoder2api"
claude --model smodel

# Windows PowerShell
$env:ANTHROPIC_BASE_URL="http://127.0.0.1:7865"
$env:ANTHROPIC_AUTH_TOKEN="sk-qoder2api"
claude --model smodel
```

### 4. 常见第三方客户端配置（NextChat / Cherry Studio / Cursor）

- **NextChat / ChatGPT-Next-Web**：
  - 接口地址：`http://127.0.0.1:7865`
  - API Key：`sk-qoder2api`
  - 自定义模型：`qwen3.8-flash,sonus,cantus,deepseek-v4-pro,glm-5.3`
- **Cherry Studio**：
  - 添加提供商：选择 `OpenAI`
  - API 基础地址：`http://127.0.0.1:7865/v1`
  - API 密钥：`sk-qoder2api`
- **Cursor / Cline / Roo Code**：
  - 选择 OpenAI API 兼容模式，设置 Base URL 为 `http://127.0.0.1:7865/v1` 即可。

---

## 🧭 模型路由与代号速查表

| 模型展示名称 | 官方后端 Key | 所属域 | 计费倍率 | 兼容别名 (Aliases) | 典型适用场景 |
| --- | --- | --- | --- | --- | --- |
| **Qwen3.8-Flash** | `qfmodel` | 国内版 / 国际版 | **0.00x (免费)** | `qwen3.8-flash` | 超高速免费日常对话、多模态图文理解 |
| **Sonus** | `smodel` | 国际版 | 8.00x | `claude-3-7-sonnet`, `claude-sonnet-4` | Claude 3.7 Sonnet，代码重构与复杂逻辑 |
| **Cantus** | `cmodel` | 国际版 | 4.00x | `claude-opus-4`, `cantus` | Claude Opus 级别顶级智能推理 |
| **Qwen3.8-Max** | `qmodel_38max` | 国际版 | 0.50x | `qwen3.8-max`, `qwen3.8-max-preview` | 超长上下文综合大模型 |
| **DeepSeek-V4-Pro** | `dmodel` | 国际版 | 0.50x | `deepseek-v4-pro`, `deepseek-chat` | 深度思维链推理、数学分析与高难度编程 |
| **DeepSeek-Flash** | `dfmodel` | 国际版 | 0.10x | `deepseek-flash`, `deepseek-v4-flash` | 极低成本的高效代码辅助 |
| **GLM-5.3** | `gmodel` | 国际版 | 0.80x | `glm-5.3` | 中文通用创作与知识问答 |
| **Kimi-K3** | `kmodel_latest` | 国际版 | 1.40x | `kimi-k3`, `kimi-k3.1-pro` | 超长文本检索与深度总结 |
| **MiniMax-M3** | `mmodel` | 国际版 | 0.20x | `minimax-m3` | 角色扮演与拟人化交互 |
| **Auto** | `auto` | 国际版 | 0.50x | `auto`, `qoder-auto` | 官方自适应动态路由 |
| **Ultimate** | `ultimate` | 国际版 | 2.00x | `ultimate` | 旗舰级综合性能模式 |
| **Qwen3.7-Flash** | `q37fmodel` | 国内版 | 0.10x | `qwen3.7-flash` | 国内专属高性价比通义模型 |
| **GLM-5.2** | `gm51model` | 国内版 | 0.60x | `glm-5.2` | 国内版智谱大模型 |

---

## 💡 账号体系与常见问题避坑指南

### 1. 国内版 (CN) 与国际版 (Global) 的额度机制差异

- **🇨🇳 Qoder 国内版（`qoder.com.cn` / 通义灵码）**：
  - 面向国内开发者，个人账号**自带基础免费算力**与免费通义千问模型。
  - **建议**：如果希望免订阅直接免费使用，请在控制台优先选择 **“🇨🇳 授权国内版账号”**。
- **🌍 Qoder 国际版（`qoder.com`）**：
  - 采用海外 SaaS 商业化模式（订阅制）。如果个人账号未在官方开通 Pro 订阅且没有可用算力包（余额为 `0.0`），调用官方接口会直接返回：
    `{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`
  - **解决方式**：在 Qoder 国际版官网开通订阅或申领新用户试用，或者直接接入国内版账号。

### 2. 报错 `503 no_healthy_account` 是什么原因？

- **原因**：账号池中目前没有可用账号（总可用账号数为 0）。
- **解决方式**：打开控制台 [http://127.0.0.1:7865/console](http://127.0.0.1:7865/console)，点击 **“🇨🇳 授权国内版账号”** 或 **“🔍 扫描本地凭据”** 导入至少一个正常账号即可。

### 3. 切勿将 OAuth 授权链接误当成 Token 导入

- 点击授权后，系统会自动打开官方授权网页并提供二维码。**只需在浏览器中点击“确认授权”（或手机扫码）即可，前端会自动轮询完成入库**。
- 切勿将浏览器地址栏中的 `https://qoder.com/device/selectAccounts?...` 链接复制并粘贴到批量导入框中。网关内置了格式校验保护机制，避免误将 URL 当作 Token 导致上游返回 500。

---

## 📁 目录架构说明

```text
Qoder2Api/
├── cmd/server/            # 应用程序主入口、CLI 命令子系统、Windows WebView2 原生 GUI 封装
├── internal/
│   ├── auth/              # 多格式凭证解析器 (Desktop / CLI / OAuth / PAT)、原子落盘保护
│   ├── cosy/              # 官方 COSY 通信协议加解密、签名与本地 Desktop/CLI 凭证探测
│   ├── pool/              # 三因子加权调度池、临期积分优先调度、自动熔断与软限频退避
│   ├── upstream/          # Qoder 国内版/国际版/VPC 客户端、模型矩阵、活动签到与额度解析
│   ├── scheduler/         # 北京时间 (CST UTC+8) 自动化活动签到、Pro 权益核验与 Token 保活
│   ├── server/            # OpenAI / Anthropic / Codex 协议处理器、赛博玻璃控制台与 RBAC 用户中心
│   ├── session/           # 会话 Sticky 粘性路由管理器
│   ├── redisstore/        # Upstash Redis 可选云端状态同步与分布式租约
│   ├── usermgr/           # 多租户管理、用户级独立 API Key 与 RPM 频控
│   └── dshmgr/            # DeepSeek Harness (DSH) 原生 Agent 运行时集成
├── config/                # 预设配置文件模板与补丁示例
├── .github/workflows/     # 自动化 CI 测试、多平台构建发布与 Docker 构建流水线
├── config.example.json    # 开箱即用的配置文件模板
├── Dockerfile             # 极简轻量级多架构 Docker 构建文件
├── docker-compose.yml     # Docker Compose 一键启动编排配置
├── DOCUMENTATION.md       # 详细的技术架构与配置手册
└── README.md              # 项目中文介绍与快速上手指南
```

---

## 📄 开源许可证

本项目基于 [MIT License](LICENSE) 开源发布。仅供个人学习、技术研究与接口自动化集成使用，请勿用于非法用途。
