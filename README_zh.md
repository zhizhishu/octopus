<div align="center">

<img src="web/public/logo.svg" alt="Octopus Logo" width="120" height="120">

### Octopus

**为个人打造的简单、美观、优雅的 LLM API 聚合与负载均衡服务**

简体中文 | [English](README.md)

</div>

> 说明：`zhizhishu/octopus` 是个人向 LLM 网关。主打 **claude-code / Codex CLI 形态保真**（出站请求重建成真实 CLI 形态，部署即生效）、**容量感知调度**（健康分层 + 容量评分 + 三态熔断）、**全 provider 互转**（OpenAI / Anthropic / Gemini / Volcengine / DeepSeek / GLM 智谱）、**多模态贯通**（图片 / 文档·PDF / 音频 / 图像生成）、**推理思考透传**（claude `max` 档 / Gemini thinkingBudget / DeepSeek `reasoning_content` / GLM `thinking`）、**IDE 工具兼容**（`count_tokens` / computer-use 内置工具 / Cursor·Cline·Continue·Claude Code）；并保留缓存稳定性修复、自动模型分组、真实多用户管理、月卡/兑换码、New API 活跃用户迁移、方案组模型映射、四层提示词管理、独立 API Key、模型单测/并发和 GHCR 公开镜像发布。详见 [README_FUTURE.md](README_FUTURE.md)。


## ✨ 特性

- 🔀 **多渠道聚合** - 支持接入多个 LLM 供应商渠道，统一管理
- 🔑 **多Key支持** - 单渠道支持配置多 Key
- ⚡ **智能优选** - 单渠道多端点，智能选择延迟最小的端点请求
- ⚖️ **负载均衡** - 自动分配请求，确保服务稳定高效
- 🔄 **协议互转** - 支持 OpenAI Chat / OpenAI Responses / Anthropic Messages / Gemini generateContent API 格式互相转换
- 💰 **价格同步** - 自动更新模型价格
- 🔃 **模型同步** - 自动与渠道同步可用模型列表，省心省力
- 📊 **数据统计** - 全面的请求统计、Token 消耗、费用追踪
- 🎨 **优雅界面** - 简洁美观的 Web 管理面板
- 🗄️ **多数据库支持** - 支持 SQLite、MySQL、PostgreSQL
- 🧭 **增强管理** - 提供独立 API Key 和提示词管理页面、API Key 用法引导、端点权限、New API 活跃用户迁移与异步 Job 迁移页、请求模型路由/计费、缓存率统计


## 🚀 快速开始

### 🐳 Docker 运行

镜像建议使用 GHCR，并把宿主机 `5050` 映射到应用内部 `8080`：

```bash
mkdir -p /root/octopus
cd /root/octopus
cat > docker-compose.yml <<'EOF'
services:
  octopus:
    image: ghcr.io/zhizhishu/octopus:future
    container_name: octopus
    restart: unless-stopped
    ports:
      - "5050:8080"
    volumes:
      - ./data:/app/data
EOF
docker compose pull
docker compose up -d
```

访问 `http://服务器IP:5050`。

直接运行

```bash
docker run -d --name octopus -v /path/to/data:/app/data -p 8080:8080 ghcr.io/zhizhishu/octopus:future
```

或者使用 docker compose 运行

```bash
wget https://raw.githubusercontent.com/zhizhishu/octopus/refs/heads/future/docker-compose.yml
docker compose up -d
```


### 📦 从 Release 下载

从 [Releases](https://github.com/zhizhishu/octopus/releases) 下载对应平台的二进制文件，然后运行：

```bash
./octopus start
```

### 🛠️ 源码运行

**环境要求：**
- Go 1.24.4
- Node.js 18+
- pnpm

```bash
# 克隆项目
git clone https://github.com/zhizhishu/octopus.git
cd octopus
# 构建前端
cd web && pnpm install && pnpm run build && cd ..
# 移动前端产物到 static 目录
mv web/out static/
# 启动后端服务
go run main.go start 
```

> 💡 **提示**：前端构建产物会被嵌入到 Go 二进制文件中，所以必须先构建前端再启动后端。

**开发模式**

```bash
cd web && pnpm install && NEXT_PUBLIC_API_BASE_URL="http://127.0.0.1:8080" pnpm run dev
## 新建终端,启动后端服务
go run main.go start
## 访问前端地址
http://localhost:3000
```

### 🔐 默认账户

首次启动后，访问 http://localhost:8080 使用以下默认账户登录管理面板：

- **用户名**：`admin`
- **密码**：`admin`

> ⚠️ **安全提示**：请在首次登录后立即修改默认密码。

### 📝 配置文件

配置文件默认位于 `data/config.json`，首次启动时自动生成。

**完整配置示例：**

```json
{
  "server": {
    "host": "0.0.0.0",
    "port": 8080
  },
  "database": {
    "type": "sqlite",
    "path": "data/data.db"
  },
  "log": {
    "level": "info"
  }
}
```

**配置项说明：**

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `server.host` | 监听地址 | `0.0.0.0` |
| `server.port` | 服务端口 | `8080` |
| `database.type` | 数据库类型 | `sqlite` |
| `database.path` | 数据库连接地址 | `data/data.db` |
| `log.level` | 日志级别 | `info` |

**数据库配置：**

支持三种数据库：

| 类型 | `database.type` | `database.path` 格式 |
|------|-----------------|---------------------|
| SQLite | `sqlite` | `data/data.db` |
| MySQL | `mysql` | `user:password@tcp(host:port)/dbname` |
| PostgreSQL | `postgres` | `postgresql://user:password@host:port/dbname?sslmode=disable` |

**MySQL 配置示例：**

```json
{
  "database": {
    "type": "mysql",
    "path": "root:password@tcp(127.0.0.1:3306)/octopus"
  }
}
```

**PostgreSQL 配置示例：**

```json
{
  "database": {
    "type": "postgres",
    "path": "postgresql://user:password@localhost:5432/octopus?sslmode=disable"
  }
}
```

> 💡 **提示**：MySQL 和 PostgreSQL 需要先手动创建数据库，程序会自动创建表结构。

**环境变量：**

所有配置项均可通过环境变量覆盖，格式为 `OCTOPUS_` + 配置路径（用 `_` 连接）：

| 环境变量 | 对应配置项 |
|----------|-----------|
| `OCTOPUS_SERVER_PORT` | `server.port` |
| `OCTOPUS_SERVER_HOST` | `server.host` |
| `OCTOPUS_DATABASE_TYPE` | `database.type` |
| `OCTOPUS_DATABASE_PATH` | `database.path` |
| `OCTOPUS_LOG_LEVEL` | `log.level` |
| `OCTOPUS_GITHUB_PAT` | 用于获取最新版本时的速率限制(可选) |
| `OCTOPUS_RELAY_MAX_SSE_EVENT_SIZE` | 最大 SSE 事件大小(可选) |
| `OCTOPUS_IMAGES_BODY_MEMORY_THRESHOLD_MB` | Images 请求体内存缓存阈值，超过阈值会落盘临时文件(可选，默认 16) |
| `OCTOPUS_IMAGES_BODY_MAX_MB` | Images 请求体最大大小限制，超过限制将拒绝请求(可选，默认 256) |
| `OCTOPUS_IMAGES_BODY_TMP_DIR` | Images 请求体临时文件目录(可选，默认 `./cache`) |
| `OCTOPUS_IMAGES_BODY_TMP_CLEANUP_HOURS` | 启动时清理临时文件的时间阈值(可选，默认 24) |


## 📸 界面预览

### 🖥️ 桌面端

<div align="center">
<table>
<tr>
<td align="center"><b>首页</b></td>
<td align="center"><b>渠道</b></td>
<td align="center"><b>分组</b></td>
</tr>
<tr>
<td><img src="web/public/screenshot/desktop-home.png" alt="首页" width="400"></td>
<td><img src="web/public/screenshot/desktop-channel.png" alt="渠道" width="400"></td>
<td><img src="web/public/screenshot/desktop-group.png" alt="分组" width="400"></td>
</tr>
<tr>
<td align="center"><b>价格</b></td>
<td align="center"><b>日志</b></td>
<td align="center"><b>设置</b></td>
</tr>
<tr>
<td><img src="web/public/screenshot/desktop-price.png" alt="价格" width="400"></td>
<td><img src="web/public/screenshot/desktop-log.png" alt="日志" width="400"></td>
<td><img src="web/public/screenshot/desktop-setting.png" alt="设置" width="400"></td>
</tr>
</table>
</div>

### 📱 移动端

<div align="center">
<table>
<tr>
<td align="center"><b>首页</b></td>
<td align="center"><b>渠道</b></td>
<td align="center"><b>分组</b></td>
<td align="center"><b>价格</b></td>
<td align="center"><b>日志</b></td>
<td align="center"><b>设置</b></td>
</tr>
<tr>
<td><img src="web/public/screenshot/mobile-home.png" alt="移动端首页" width="140"></td>
<td><img src="web/public/screenshot/mobile-channel.png" alt="移动端渠道" width="140"></td>
<td><img src="web/public/screenshot/mobile-group.png" alt="移动端分组" width="140"></td>
<td><img src="web/public/screenshot/mobile-price.png" alt="移动端价格" width="140"></td>
<td><img src="web/public/screenshot/mobile-log.png" alt="移动端日志" width="140"></td>
<td><img src="web/public/screenshot/mobile-setting.png" alt="移动端设置" width="140"></td>
</tr>
</table>
</div>


## 📖 功能说明

### 📡 渠道管理

渠道是连接 LLM 供应商的基础配置单元。

**Base URL 说明：**

程序会根据渠道类型自动补全 API 路径，您只需填写基础 URL 即可：

| 渠道类型 | 自动补全路径 | 填写 URL | 完整请求地址示例 |
|----------|-------------|----------|-----------------|
| OpenAI Chat | `/chat/completions` | `https://api.openai.com/v1` | `https://api.openai.com/v1/chat/completions` |
| OpenAI Responses | `/responses` | `https://api.openai.com/v1` | `https://api.openai.com/v1/responses` |
| OpenAI Images | `/images/generations`、`/images/edits`、`/images/variations` | `https://api.openai.com/v1` | `https://api.openai.com/v1/images/generations` |
| OpenAI 原样兼容 | `/completions`、`/edits`、`/responses/compact`、`/audio/*`、`/moderations`、`/rerank` | `https://api.openai.com/v1` | `https://api.openai.com/v1/audio/speech` |
| Anthropic | `/messages` | `https://api.anthropic.com/v1` | `https://api.anthropic.com/v1/messages` |
| Gemini | `/models/:model:generateContent` | `https://generativelanguage.googleapis.com/v1beta` | `https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent` |

> 💡 **提示**：填写 Base URL 时无需包含具体的 API 端点路径，程序会自动处理。

**渠道操作与高级调度：**

- **便捷操作与本地搜索**：渠道列表卡片与编辑弹窗顶部提供编辑、克隆与删除快捷操作；支持在渠道列表进行本地模糊搜索（基于安全考量，不搜索 Key 明文）。
- **多 Key 并发竞速**：
  - 默认**关闭**。开启后，在同渠道内选择 2 至 5 个可用 Key 并发或错峰对冲发起请求。
  - 支持配置对冲延迟（Hedge Delay）；首个有效响应胜出并立即返回下游，其余落后/失败请求自动取消。
  - 图像与视频生成请求不参与竞速；竞速可能产生少量额外的上游 Token/调用消耗。
  - **说明**：多 Key 并发竞速与渠道的「关闭熔断与冷却」为两个独立功能，互不影响。
- **日志拦截与挂起请求自动救援**：
  - 当上游临时失败被网关拦截挂起时，系统默认由机器按访问计划（轮询/优先填充）自动执行多轮退避救援（自动重读最新渠道与 Key 配置、指数退避、下游连接保持）。
  - 控制台的「立即指定重试 / 人工选择」属于立即覆盖操作，并非必需的手工步骤。

---

### 📁 分组管理

分组用于将多个渠道聚合为一个统一的对外模型名称。

**核心概念：**

- **分组名称** 即程序对外暴露的模型名称
- 调用 API 时，将请求中的 `model` 参数设置为分组名称即可

**负载均衡模式：**

| 模式 | 说明 |
|------|------|
| 🔄 **轮询** | 每次请求依次切换到下一个渠道 |
| 🎲 **随机** | 每次请求随机选择一个可用渠道 |
| 🛡️ **故障转移** | 优先使用高优先级渠道，仅当其故障时才切换到低优先级渠道 |
| ⚖️ **加权分配** | 根据渠道设置的权重比例分配请求 |

> 💡 **示例**：创建分组名称为 `gpt-4o`，将多个供应商的 GPT-4o 渠道加入该分组，即可通过统一的 `model: gpt-4o` 访问所有渠道。

---

### 💰 价格管理

管理系统中的模型价格信息。

**数据来源：**

- 系统会定期从 [models.dev](https://github.com/sst/models.dev) 同步更新模型价格数据
- 当创建渠道时，若渠道包含的模型不在 models.dev 中，系统会自动在此页面创建该模型的价格信息,所以此页面显示的是没有从上游获取到价格的模型，用户可以手动设置价格
- 也支持手动创建 models.dev 中已存在的模型，用于自定义价格

**价格优先级：**

| 优先级 | 来源 | 说明 |
|:------:|------|------|
| 🥇 高 | 本页面 | 用户在价格管理页面设置的价格 |
| 🥈 低 | models.dev | 自动同步的默认价格 |

> 💡 **提示**：如需覆盖某个模型的默认价格，只需在价格管理页面为其设置自定义价格即可。

---

### 📋 请求日志

系统提供完整的请求审计与调试支持：

- **模糊搜索**：支持按用户名、API Key 名称、请求模型、实际上游模型、渠道名称、端点名称、请求路径、会话 Key、错误信息及错误状态码进行跨字段模糊搜索；纯数字输入额外精准匹配日志 ID 或渠道 ID。
- **多维筛选**：支持按入口端点协议（Chat / Responses / Messages / Gemini 等）、厂商系列（OpenAI / Anthropic / Gemini / DeepSeek 等）、以及具体模型名称精确筛选。
- **救援与详情审计**：
  - 上游故障请求进入挂起状态时，日志界面展示自动救援状态（轮询轮次、退避倒计时），并支持管理员随时点击「立即指定重试」覆盖救援策略。
  - 管理员可查看完整请求/响应详情、渠道尝试链、Token 统计、费用快照及耗时。

---

### ⚙️ 设置

系统全局配置项。

**统计保存周期（分钟）：**

由于程序涉及大量统计项目，若每次请求都直接写入数据库会影响读写性能。因此程序采用以下策略：

- 统计数据先保存在 **内存** 中
- 按设定的周期 **定期批量写入** 数据库

> ⚠️ **重要提示**：退出程序时，请使用正常的关闭方式（如 `Ctrl+C` 或发送 `SIGTERM` 信号），以确保内存中的统计数据能正确写入数据库。**请勿使用 `kill -9` 等强制终止方式**，否则可能导致统计数据丢失。




## 🔌 客户端接入

### OpenAI SDK

```python
from openai import OpenAI
import os

client = OpenAI(   
    base_url="http://127.0.0.1:8080/v1",   
    api_key="sk-octopus-xxxxxxxxxxxxxxxxxxxxxxxx", 
)
completion = client.chat.completions.create(
    model="octopus-openai",  // 填写正确的分组名称
    messages = [
        {"role": "user", "content": "Hello"},
    ],
)
print(completion.choices[0].message.content)
```

### Claude Code

编辑 `~/.claude/settings.json`

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080",
    "ANTHROPIC_AUTH_TOKEN": "sk-octopus-xxxxxxxxxxxxxxxxxxxxxxxx",
    "API_TIMEOUT_MS": "3000000",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "ANTHROPIC_MODEL": "octopus-sonnet-4-5",
    "ANTHROPIC_SMALL_FAST_MODEL": "octopus-haiku-4-5",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "octopus-sonnet-4-5",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "octopus-sonnet-4-5",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "octopus-haiku-4-5"
  }
}
```

### Codex

编辑 `~/.codex/config.toml`

```toml
model = "octopus-codex" # 填写正确的分组名称

model_provider = "octopus"

[model_providers.octopus]
name = "octopus"
base_url = "http://127.0.0.1:8080/v1"
```
编辑 `~/.codex/auth.json`

```json
{
  "OPENAI_API_KEY": "sk-octopus-xxxxxxxxxxxxxxxxxxxxxxxx"
}
```


---

## 🤝 致谢

- 🙏 [looplj/axonhub](https://github.com/looplj/axonhub) - 本项目的 LLM API 适配模块直接源自该仓库的实现
- 📊 [sst/models.dev](https://github.com/sst/models.dev) - AI 模型数据库，提供模型价格数据
