# 开发与测试（Development）

本文说明如何构建、运行测试，以及哪些东西是"生成产物"而不该进版本库。命令以 CI（`.github/workflows/future-build.yml`）为准；正式验证在 Linux 环境（CI 或 Linux 主机/容器）按同一顺序执行。

## 工具链

| 项 | 依据 | 说明 |
|---|---|---|
| Go | `go.mod` | 以 `go.mod` 为准，不要按别处的旧数字装 Go |
| Node.js | CI 使用 24 | 版本按 CI，lockfile 不定义 Node 版本 |
| pnpm | `web/pnpm-lock.yaml` | 用 pnpm；依赖版本由该 lockfile 锁定，CI 用 `pnpm install --frozen-lockfile` |
| Python | CI 镜像任务会安装 3.13 | 手工 `go build` / `go test` 不需要 Python |

## 前端（`web/`）

Next.js App Router，`output: "export"` 静态导出，产物目录为 `web/out`。

```bash
cd web
pnpm install --frozen-lockfile   # 与 lockfile 严格一致
pnpm test                        # node --test tests/*.test.cjs
pnpm run build                   # next build → web/out
```

可选静态检查：`pnpm lint`（`eslint`）是仓库脚本，但不在当前 CI 的必跑步骤里。

开发模式（前端热更新，后端单独跑）：

```bash
cd web
NEXT_PUBLIC_API_BASE_URL="http://127.0.0.1:8080" pnpm run dev   # http://localhost:3000
```

构建期还会读取两个可选变量：`NEXT_PUBLIC_APP_VERSION`（版本号）与 `NEXT_PUBLIC_GITHUB_REPO`（仓库地址）；不设置时前端回退到内置默认值。

## 构建次序（重要）

前端产物通过 `//go:embed all:out` 编译进二进制，所以**前端必须先出物，且后端命令要在仓库根执行**。

干净 Linux 检出上的最短完整序列（仅适用于干净检出；把 `/path/to/octopus` 换成自己的检出路径，不要用它覆盖已有的 `data/`）：

```bash
set -e
# ① 仓库根（含 go.mod 的目录）
cd /path/to/octopus
cd web && pnpm install --frozen-lockfile && pnpm run build   # 产物: web/out

# ② 回到仓库根
cd ..

# ③ 让 static/out 等于这次产物；干净检出里 static/out 不存在
mkdir -p static
test ! -e static/out        # 已存在则在此失败停下，改走 CI 受控替换
mv web/out static/out

# ④ 仍在仓库根：先测试再构建
mkdir -p build
go test -count=1 ./...
go build -o build/octopus .
```

若 `static/out` 已经存在（之前构建过），上面的 `test ! -e static/out` 会直接报错停下；此时改用 CI 的受控替换流程（见该 workflow 的 Embed 步骤，先移除旧产物再放入新产物），不要在本机手工删除目录。

仓库里**不包含生成产物**：`static/out` 下的构建产物未被跟踪。所以跳过 ①–③ 直接 `go build` / `go test`，会因嵌入目录不存在而失败——这不是代码问题，是次序问题。

其他注意事项：

- 不要在仓库根目录做"清空/重置"式的批量删除：`data/` 存放运行期配置与数据库，删了会丢自己的数据。
- 只关心后端时，也可以把 ①–③ 交给 CI 或容器完成，再在 Linux 环境里跑 ④。

## 后端（Go）

命令见上面的构建次序块（相同两条，不重复维护）。版本/作者/提交等元信息通过 `-ldflags -X .../internal/conf.*` 注入，打包脚本 `scripts/build.sh` 里有完整写法；自行 `go build` 时不注入也能跑，只是版本号显示为默认值。

## 测试分布

| 类型 | 位置 | 跑法 |
|---|---|---|
| 后端单元/回归 | `internal/**/*_test.go` | `go test -count=1 ./...` |
| 前端脚本级回归 | `web/tests/*.test.cjs`（`node --test`） | `cd web && pnpm test` |
| CI 编排 | `.github/workflows/future-build.yml` | push/PR 到 `future` 分支，或手动触发 |

CI 的顺序是：装依赖 → 前端回归 → 前端构建 → 产物就位 → Go 测试 → 二进制 → 镜像。镜像任务的条件是非 PR **且** ref 为 `refs/heads/future`，因此 `future` 分支的 push 和满足该 ref 的手动触发都可能发布镜像；PR 不发布。

## 生成产物、源码与测试夹具

| 类别 | 例子 | 是否入库 |
|---|---|---|
| 源码 | `internal/`, `web/src/`, `cmd/`, `main.go` | 是 |
| 生成产物 | `web/out/`、`static/out/` 内容、`build/`、`test-results/` | 否（构建时生成） |
| 运行期数据 | `data/`（配置与默认 SQLite 数据库）、临时缓存目录 | 否 |
| 测试夹具 | 需要 `go:embed` 打进二进制、且按字节/清单校验的基准文件 | 是（例外，且须保持原始字节） |

夹具类文件通常依赖 `.gitattributes` 保持原始换行/字节，改动它们前先看该文件，避免校验失败。

## 用户自用配置的隔离

- 运行配置默认 `./data/config.json`，首次启动自动生成；**默认 SQLite** 数据库文件也在这个目录。改用外部 MySQL / PostgreSQL 时，数据库不在该目录（连接串在配置里）。这些都属**部署环境**，不要提交。
- 可覆盖项走环境变量（`OCTOPUS_` 前缀 + 配置路径），密钥/令牌请放在未入库的位置，不要写进示例配置或文档。
- 本文与示例里的地址一律用 `localhost` / 占位域名，不要贴真实主机、IP、账号或令牌。

## 验证环境

- 构建、镜像与全量回归以 Linux（CI 或 Linux 主机/容器）为准；本文的结论都建立在该环境上。
- Windows 仅用于编辑代码，不跑任何 Go 测试 / 前端类型检查 / 构建——这些全部放在 Linux 环境（CI 或 Linux 主机/容器）；容器相关操作同样放在 Linux 环境，不在 Windows 上做 Docker 构建/运行。

## 提交前

1. Linux 环境（CI 或 Linux 主机/容器）里前端回归与 Go 测试通过。
2. 改动主题单一，AI 参与内容已人工审查（见 [../CONTRIBUTING.md](../CONTRIBUTING.md)）。
3. 新增文档若要公开，确认 `.gitignore` 已精确放行；`docs/` 根层的 md 默认不入库。
4. 架构方位见 [architecture-overview.md](architecture-overview.md)。


## 日志压力保护的边界

日志正文的存储副本在剥离图片和脱敏后按列限制为 1 MiB，超长标记计入上限；真正转发的请求和响应不被截断。只为最终可见的图片占位符建立图片存储引用。依赖完整文本的被动探针在日志明确截断时跳过，不把不完整样本当作模型异常证据。

待落库队列同时受 500 条和 64 MiB payload 估值约束；后台 flush 持有的快照另受相同预算约束。它不是进程 RSS 上限，也不是所有在途数据合计只有 64 MiB。超过预算的单条不挤掉其它正常记录；持续积压可能淘汰最旧的日志，已有健康计数会显示丢弃/写入失败。

可测量本地磁盘余量时，SQLite 日志后台写入保留 256 MiB 加批次估值裕量，日志图片入队与实际写盘各复核一次余量。触发压力时暂停这些可选写入，保留有界队列及限频告警；不拒绝业务转发、不主动删除已存数据。无法测量的平台维持原有写入行为。该检查不是文件系统事务，不能防止其它进程抢占磁盘，不能代替保留期、容量配置、WAL 管理、监控和备份。

用户周期限额的迁移与回退见 [user-limits.md](user-limits.md)。发布镜像之后仍需按部署环境单独核对数据库迁移、磁盘余量和待结算任务。
