# 架构概览（Architecture Overview）

本文只描述本仓库**公开源码**中的职责划分与主要入口，供改代码的人建立方位感。具体调用链、路由清单与文件数量以源码为准，本文不复制它们。

## 定位

Octopus 是一个面向个人使用的多渠道 LLM API 聚合与负载均衡网关：

- **对外**：用一套端点接收 OpenAI Chat / OpenAI Responses / Anthropic Messages / Gemini 原生请求，按分组与路由策略选择上游渠道。
- **对内**：协议转换（inbound → 统一内部请求 → outbound）、渠道/键/分组管理、请求日志与用量统计、多用户与访问方案。
- **交付形态**：Go 后端 + 前端静态产物，编译进**单个二进制**；运行期数据落在本地数据目录（见 [development.md](development.md)）。

技术栈以 `go.mod` 与 `web/package.json` 为准：Go（Gin、cobra、viper、GORM + SQLite/MySQL/PostgreSQL）、Next.js App Router（`output: "export"` 静态导出）+ React + TypeScript + Tailwind。

## 主要入口

```
main.go                     // 薄壳，仅调用 cmd.Execute()
└─ cmd/                     // cobra 命令；start 子命令是服务入口
   ├─ root.go               // 命令注册
   └─ start.go              // 初始化顺序（见下）
```

`start` 的初始化顺序（`cmd/start.go`）：

1. `conf.Load` 读配置（默认 `./data/config.json`，首次启动自动生成）
2. `db.InitDB` 打开数据库
3. `op.UserInit` 初始化用户/管理员
4. `op.InitCache` 加载运行期缓存（退出时 `op.SaveCache` 落盘）
5. `server.Start` 装配 HTTP
6. `task.Init` + 后台任务循环
7. `shutdown.Listen` 阻塞等待信号

数据库、用户、缓存初始化及 HTTP 服务启动返回错误时，`start` 会记录日志并非零退出；配置加载的返回错误当前未在调用处检查，因此这里不承诺全部初始化失败都会退出。

## HTTP 装配

`internal/server/server.go`：

- `gin.New()` + 中间件（CORS、嵌入式前端静态资源），debug 模式额外挂 logger
- `router.RegisterAll(r)` 注册所有路由
- 入站观测：`telemetry` 包装 handler 与连接上下文，旁路记录入站请求头原始顺序（只观察，不改写出站）
- `net.Listen` **同步**取监听器，绑定失败立即返回错误

路由采用**自注册**：各 handler 包在 `init()` 里调用 `router.NewGroupRouter(...)` 把路由描述登记进全局表（`internal/server/router/router.go`），`RegisterAll` 再把描述表注册到 gin 引擎。登记表是**描述**而非一次性队列，注册后不清空——同一进程内多个引擎（测试、子引擎）都能拿到完整路由。

## 分层职责

| 位置 | 职责 |
|---|---|
| `internal/transformer/` | 协议转换。`inbound/{anthropic,openai,gemini}` 解析客户端请求为统一内部请求，`outbound/*` 再序列化成目标协议 |
| `internal/relay/` | 转发编排：通道/键选择、上游请求构造与发送、流式转发、重试与救援、图像/视频等特殊通道 |
| `internal/relay/balancer/` | 选路阶段的负载均衡与熔断等决策（具体策略以源码为准） |
| `internal/relay/bodycache/` | 请求体缓存与超大请求体暂存 |
| `internal/client/` | 出站 HTTP 传输，承载与协议/身份相关的传输约束；改动需做兼容性验证 |
| `internal/server/` | 装配、路由、中间件、`handlers/` 按域拆分的 HTTP 入口 |
| `internal/op/` | 运营层：各域 CRUD、缓存、统计与排名（有状态，注意并发） |
| `internal/db/` + `internal/model/` | 存储层与数据结构 |
| `internal/task/` | 后台任务（渠道/模型同步、审计落盘等） |
| `internal/conf/` | 配置结构、加载、版本元信息 |
| `static/` | 前端产物嵌入（`//go:embed all:out`） |
| `web/` | 前端：`app/` 外壳、`api/`（客户端与类型）、`components/modules/` 业务模块、`stores/`、`hooks/`、`lib/` |

## 两类接口面

- **转发面**（无前端）：转换类端点（Chat / Responses / Messages / Embedding）在 `internal/server/handlers/relay.go` 进入 `relay.Handler`；Gemini 原生端点由 `internal/server/handlers/gemini.go` 进入同一个 `relay.Handler`。图像、视频与透传类端点由各自的 handler 处理，不经过这条统一分派。
- **管理面**：`/api/v1/...`，按域分布在 `internal/server/handlers/`，前端 `web/src/api/endpoints/` 与之同域对应。

## 边界说明

- **前端与后端是两条构建链**：`web` 先构建出静态目录，再由 `static/` 嵌入。未构建前端时，`go build` / `go test` 会因为嵌入目录缺失而失败——构建次序见 [development.md](development.md)。
- **配置与数据属于部署环境**：默认 `data/config.json` 与默认 SQLite 数据库文件都在数据目录，不入库、不随代码分发。
- **"身份/形态"是请求构造的实现机制**：出站请求会按目标上游的协议形态重建头部与请求体，目的是让上游正常处理请求；这不等于保证与任何真实客户端逐字节一致，也不承诺上游行为。
- **`docs/` 根层的 md 默认不入库**：`.gitignore` 上有 `docs/*.md` 匹配规则，仅个别公开文档（含本文与 `development.md`）以精确例外放行；该规则只针对 `docs/` 根层文件，不影响其他目录。新增文档若要公开，需要在 `.gitignore` 里单独放行。

## 延伸阅读

- [development.md](development.md) — 工具链、构建次序、测试与产物隔离
- [../README.md](../README.md) / [../README_zh.md](../README_zh.md) — 安装、配置与客户端接入
- [../README_FUTURE.md](../README_FUTURE.md) — 版本亮点叙述
- [../CONTRIBUTING.md](../CONTRIBUTING.md) — 提交范围与审查要求
