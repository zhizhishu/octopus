<div align="center">

<img src="web/public/logo.svg" alt="Octopus Logo" width="120" height="120">

### Octopus

**A Simple, Beautiful, and Elegant LLM API Aggregation & Load Balancing Service for Individuals**

 English | [简体中文](README_zh.md)

</div>

> [!NOTE]
> **Octopus**：由 `zhizhishu/octopus` 维护的个人向 LLM 网关。主打 **claude-code / Codex CLI 形态保真**（出站请求重建成真实 CLI 形态，部署即生效）、**容量感知调度**（健康分层 + 容量评分 + 三态熔断）、**全 provider 互转**（OpenAI / Anthropic / Gemini / Volcengine / DeepSeek / GLM 智谱）、**多模态贯通**（图片 / 文档·PDF / 音频 / 图像生成）、**推理思考透传**（claude `max` 档 + 预算钳制 / Gemini thinkingBudget / DeepSeek `reasoning_content` / GLM `thinking`）、**IDE 工具兼容**（`count_tokens` / computer-use 内置工具 / Cursor·Cline·Continue·Claude Code）；并保留缓存稳定性修复、自动模型分组、真实多用户管理、月卡/兑换码、New API 活跃用户迁移、方案组模型映射、四层提示词管理、独立 API Key、模型单测/并发、`models.dev` 价格库入口和 GHCR 公开镜像发布。完整说明和截图见 [README_FUTURE.md](README_FUTURE.md)。
>
> <img src="docs/screenshots/future/octopus-multiuser-admin-users.png" alt="Octopus Future multi-user management preview" width="720">


## ✨ Features

- 🔀 **Multi-Channel Aggregation** - Connect multiple LLM provider channels with unified management
- 🔑 **Multi-Key Support** - Support multiple API keys for a single channel
- ⚡ **Smart Selection** - Multiple endpoints per channel, smart selection of the endpoint with the shortest delay
- ⚖️ **Load Balancing** - Automatic request distribution for stable and efficient service
- 🔄 **Protocol Conversion** - Seamless conversion between OpenAI Chat / OpenAI Responses / Anthropic Messages / Gemini generateContent formats
- 💰 **Price Sync** - Automatic model pricing updates
- 🔃 **Model Sync** - Automatic synchronization of available model lists with channels
- 📊 **Analytics** - Comprehensive request statistics, token consumption, and cost tracking
- 🎨 **Elegant UI** - Clean and beautiful web management panel
- 🗄️ **Multi-Database Support** - Support for SQLite, MySQL, PostgreSQL
- 🧭 **Enhanced Management** - Standalone API-key and prompt-management pages, copyable API-key usage guidance, endpoint-family permissions, New API active-user migration with an async admin Job page, request-model routing/billing, Route/Group setup guidance, CPA-compatible Responses-to-Chat fallback, cache-rate analytics, Responses streaming tool-call hardening, and shared UI overflow guardrails


## 🚀 Quick Start

### 🐳 Docker

For the image maintained at `zhizhishu/octopus`, use GHCR and map host port `5050` to the app's internal `8080`:

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

Open `http://SERVER_IP:5050`.

Run directly:

```bash
docker run -d --name octopus -v /path/to/data:/app/data -p 8080:8080 ghcr.io/zhizhishu/octopus:future
```

Or use docker compose:

```bash
wget https://raw.githubusercontent.com/zhizhishu/octopus/refs/heads/future/docker-compose.yml
docker compose up -d
```


### 📦 Download from Release

Download the binary for your platform from [Releases](https://github.com/zhizhishu/octopus/releases), then run:

```bash
./octopus start
```

### 🛠️ Build from Source

**Requirements:**
- Go 1.24.4
- Node.js 18+
- pnpm

```bash
# Clone the repository
git clone https://github.com/zhizhishu/octopus.git
cd octopus
# Build frontend
cd web && pnpm install && pnpm run build && cd ..
# Move frontend assets to static directory
mv web/out static/
# Start the backend service
go run main.go start 
```

> 💡 **Tip**: The frontend build artifacts are embedded into the Go binary, so you must build the frontend before starting the backend.

**Development Mode**

```bash
cd web && pnpm install && NEXT_PUBLIC_API_BASE_URL="http://127.0.0.1:8080" pnpm run dev
## Open a new terminal, start the backend service
go run main.go start
## Access the frontend at
http://localhost:3000
```

### 🔐 Default Credentials

After first launch, visit http://localhost:8080 and log in to the management panel with:

- **Username**: `admin`
- **Password**: `admin`

> ⚠️ **Security Notice**: Please change the default password immediately after first login.

### 📝 Configuration File

The configuration file is located at `data/config.json` by default and is automatically generated on first startup.

**Complete Configuration Example:**

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

**Configuration Options:**

| Option | Description | Default |
|--------|-------------|---------|
| `server.host` | Listen address | `0.0.0.0` |
| `server.port` | Server port | `8080` |
| `database.type` | Database type | `sqlite` |
| `database.path` | Database connection string | `data/data.db` |
| `log.level` | Log level | `info` |

**Database Configuration:**

Three database types are supported:

| Type | `database.type` | `database.path` Format |
|------|-----------------|-----------------------|
| SQLite | `sqlite` | `data/data.db` |
| MySQL | `mysql` | `user:password@tcp(host:port)/dbname` |
| PostgreSQL | `postgres` | `postgresql://user:password@host:port/dbname?sslmode=disable` |

**MySQL Configuration Example:**

```json
{
  "database": {
    "type": "mysql",
    "path": "root:password@tcp(127.0.0.1:3306)/octopus"
  }
}
```

**PostgreSQL Configuration Example:**

```json
{
  "database": {
    "type": "postgres",
    "path": "postgresql://user:password@localhost:5432/octopus?sslmode=disable"
  }
}
```

> 💡 **Tip**: MySQL and PostgreSQL require manual database creation. The application will automatically create the table structure.

### 🌐 Environment Variables

All configuration options can be overridden via environment variables using the format `OCTOPUS_` + configuration path (joined with `_`):

| Environment Variable | Configuration Option |
|---------------------|---------------------|
| `OCTOPUS_SERVER_PORT` | `server.port` |
| `OCTOPUS_SERVER_HOST` | `server.host` |
| `OCTOPUS_DATABASE_TYPE` | `database.type` |
| `OCTOPUS_DATABASE_PATH` | `database.path` |
| `OCTOPUS_LOG_LEVEL` | `log.level` |
| `OCTOPUS_GITHUB_PAT` | For rate limiting when getting the latest version (optional) |
| `OCTOPUS_RELAY_MAX_SSE_EVENT_SIZE` | Maximum SSE event size (optional) |
| `OCTOPUS_IMAGES_BODY_MEMORY_THRESHOLD_MB` | Images request body in-memory threshold. If exceeded, it will be spooled to a temporary file (optional, default 16) |
| `OCTOPUS_IMAGES_BODY_MAX_MB` | Images request body maximum size. Requests above this limit are rejected (optional, default 256) |
| `OCTOPUS_IMAGES_BODY_TMP_DIR` | Images request body temporary directory (optional, default `./cache`) |
| `OCTOPUS_IMAGES_BODY_TMP_CLEANUP_HOURS` | Startup cleanup threshold for temporary files (optional, default 24) |

## 📸 Screenshots

### 🖥️ Desktop

<div align="center">
<table>
<tr>
<td align="center"><b>Dashboard</b></td>
<td align="center"><b>Channel Management</b></td>
<td align="center"><b>Group Management</b></td>
</tr>
<tr>
<td><img src="web/public/screenshot/desktop-home.png" alt="Dashboard" width="400"></td>
<td><img src="web/public/screenshot/desktop-channel.png" alt="Channel" width="400"></td>
<td><img src="web/public/screenshot/desktop-group.png" alt="Group" width="400"></td>
</tr>
<tr>
<td align="center"><b>Price Management</b></td>
<td align="center"><b>Logs</b></td>
<td align="center"><b>Settings</b></td>
</tr>
<tr>
<td><img src="web/public/screenshot/desktop-price.png" alt="Price Management" width="400"></td>
<td><img src="web/public/screenshot/desktop-log.png" alt="Logs" width="400"></td>
<td><img src="web/public/screenshot/desktop-setting.png" alt="Settings" width="400"></td>
</tr>
</table>
</div>

### 📱 Mobile

<div align="center">
<table>
<tr>
<td align="center"><b>Home</b></td>
<td align="center"><b>Channel</b></td>
<td align="center"><b>Group</b></td>
<td align="center"><b>Price</b></td>
<td align="center"><b>Logs</b></td>
<td align="center"><b>Settings</b></td>
</tr>
<tr>
<td><img src="web/public/screenshot/mobile-home.png" alt="Mobile Home" width="140"></td>
<td><img src="web/public/screenshot/mobile-channel.png" alt="Mobile Channel" width="140"></td>
<td><img src="web/public/screenshot/mobile-group.png" alt="Mobile Group" width="140"></td>
<td><img src="web/public/screenshot/mobile-price.png" alt="Mobile Price" width="140"></td>
<td><img src="web/public/screenshot/mobile-log.png" alt="Mobile Logs" width="140"></td>
<td><img src="web/public/screenshot/mobile-setting.png" alt="Mobile Settings" width="140"></td>
</tr>
</table>
</div>


## 📖 Documentation

### 📡 Channel Management

Channels are the basic configuration units for connecting to LLM providers.

**Base URL Guide:**

The program automatically appends API paths based on channel type. You only need to provide the base URL:

| Channel Type | Auto-appended Path | Base URL | Full Request URL Example |
|--------------|-------------------|----------|--------------------------|
| OpenAI Chat | `/chat/completions` | `https://api.openai.com/v1` | `https://api.openai.com/v1/chat/completions` |
| OpenAI Responses | `/responses` | `https://api.openai.com/v1` | `https://api.openai.com/v1/responses` |
| OpenAI Images | `/images/generations`, `/images/edits`, `/images/variations` | `https://api.openai.com/v1` | `https://api.openai.com/v1/images/generations` |
| OpenAI Raw Compatible | `/completions`, `/edits`, `/responses/compact`, `/audio/*`, `/moderations`, `/rerank` | `https://api.openai.com/v1` | `https://api.openai.com/v1/audio/speech` |
| Anthropic | `/messages` | `https://api.anthropic.com/v1` | `https://api.anthropic.com/v1/messages` |
| Gemini | `/models/:model:generateContent` | `https://generativelanguage.googleapis.com/v1beta` | `https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent` |

> 💡 **Tip**: No need to include specific API endpoint paths in the Base URL - the program handles this automatically.

---

### 📁 Group Management

Groups aggregate multiple channels into a unified external model name.

**Core Concepts:**

- **Group name** is the model name exposed by the program
- When calling the API, set the `model` parameter to the group name

**Load Balancing Modes:**

| Mode | Description |
|------|-------------|
| 🔀 **Spread** | Capacity-aware load-balancing across same-priority channels (priority stays a hard boundary) |
| 🛡️ **Fill First** | Concentrates traffic on the top healthy channel for best upstream prompt-cache hits; sinks to the next only on trip / cooldown / rate-limit |

> 💡 **Example**: Create a group named `gpt-4o`, add multiple providers' GPT-4o channels to it, then access all channels via a unified `model: gpt-4o`.

---

### 💰 Price Management

Manage model pricing information in the system.

**Data Sources:**

- The system periodically syncs model pricing data from [models.dev](https://github.com/sst/models.dev)
- When creating a channel, if the channel contains models not in models.dev, the system automatically creates pricing information for those models on this page, so this page displays models that haven't had their prices fetched from upstream, allowing users to set prices manually
- Manual creation of models that exist in models.dev is also supported for custom pricing

**Price Priority:**

| Priority | Source | Description |
|:--------:|--------|-------------|
| 🥇 High | This Page | Prices set by user in price management page |
| 🥈 Low | models.dev | Auto-synced default prices |

> 💡 **Tip**: To override a model's default price, simply set a custom price for it in the price management page.

---

### ⚙️ Settings

Global system configuration.

**Statistics Save Interval (minutes):**

Since the program handles numerous statistics, writing to the database on every request would impact read/write performance. The program uses this strategy:

- Statistics are first stored in **memory**
- Periodically **batch-written** to the database at the configured interval

> ⚠️ **Important**: When exiting the program, use proper shutdown methods (like `Ctrl+C` or sending `SIGTERM` signal) to ensure in-memory statistics are correctly written to the database. **Do NOT use `kill -9` or other forced termination methods**, as this may result in statistics data loss.

---

## 🔌 Client Integration

### OpenAI SDK

```python
from openai import OpenAI
import os

client = OpenAI(   
    base_url="http://127.0.0.1:8080/v1",   
    api_key="sk-octopus-xxxxxxxxxxxxxxxxxxxxxxxx", 
)
completion = client.chat.completions.create(
    model="octopus-openai",  # Use the correct group name
    messages = [
        {"role": "user", "content": "Hello"},
    ],
)
print(completion.choices[0].message.content)
```

### Claude Code

Edit `~/.claude/settings.json`

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

Edit `~/.codex/config.toml`

```toml
model = "octopus-codex" # Use the correct group name

model_provider = "octopus"

[model_providers.octopus]
name = "octopus"
base_url = "http://127.0.0.1:8080/v1"
```

Edit `~/.codex/auth.json`

```json
{
  "OPENAI_API_KEY": "sk-octopus-xxxxxxxxxxxxxxxxxxxxxxxx"
}
```

---

## 🤝 Acknowledgments

- 🙏 [looplj/axonhub](https://github.com/looplj/axonhub) - The LLM API adaptation module in this project is directly derived from this repository
- 📊 [sst/models.dev](https://github.com/sst/models.dev) - AI model database providing model pricing data
