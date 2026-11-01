# SQLite WAL 体积控制：需求与决定

`data.db-wal` 为什么会一直停在 9.48 GB、我们怎么让它自己缩回去。改之前先读。
相关代码：`internal/db/db.go`（`initSQLite`、`SQLiteJournalSizeLimitBytes`）、
`internal/op/db_wal_checkpoint.go`（周期检查点）、`internal/op/log_storage.go`（`relayLogVacuum` 之后顺手截一次）、
`internal/task/init.go`（`TaskSQLiteWALCheckpoint`，每 5 分钟）。

## 现象（生产实测）

- `data.db` 22.8 GB，WAL 模式，`auto_vacuum=2`（增量）已生效。
- `data.db-wal` 恒定 9.48 GB，重启、清理、回收都不变；只有管理员手动「压缩数据库」会截。
- 约束：**不许减少日志正文的存储**——只能解决"WAL 文件不缩"，不能靠少存数据。

## 原因

1. WAL 模式下检查点只把帧搬回主库、让 WAL 从头**复用**，不截短文件（覆盖比追加快）。
   默认 `journal_size_limit = -1`（不限），所以 `-wal` 一旦被撑大，就永远停在历史最高水位。
2. 撑大的那一次：启动回填/迁移、`relayLogStoredBytes` 一类全表扫描期间存在长读者，
   WAL 无法回绕（checkpoint starvation），自动检查点只能一路往后追加。
3. `wal_autocheckpoint`（默认 1000 页）永远是 PASSIVE，不会 TRUNCATE；
   应用里唯一的 `wal_checkpoint(TRUNCATE)` 在手动 `DBCompactNow`。

## 业界怎么做（只学思路，不抄代码）

| 来源 | 做法 | 我们学到的 |
|---|---|---|
| [SQLite PRAGMA 文档](https://www.sqlite.org/pragma.html#pragma_journal_size_limit)、[WAL 文档](https://www.sqlite.org/wal.html) | `journal_size_limit`：每次 WAL 回绕/提交时把超出上限的部分截掉；`wal_checkpoint` 四种模式：PASSIVE 永不等待，FULL/RESTART/TRUNCATE 会调用 busy handler、**阻塞写者**并等读者；TRUNCATE 成功后文件为 0 字节。长读者会让 WAL 无法回绕而无限增长。 | 设 `journal_size_limit` 是根治"不缩"的官方手段；TRUNCATE 会挡写者，必须控制等待时长。 |
| [PocketBase `core/db_connect.go`](https://github.com/pocketbase/pocketbase/blob/master/core/db_connect.go) | 同样用 modernc 驱动，DSN 里 `_pragma=journal_size_limit(200000000)`；备份前跑 `wal_checkpoint(TRUNCATE)`。 | 同驱动、同 DSN 写法，限额在 ~200 MB 量级。 |
| [Litestream WAL truncate 指南](https://litestream.io/guides/wal-truncate-threshold/) / [`db.go`](https://github.com/benbjohnson/litestream/blob/main/db.go) | 三层：~4 MB 触发 PASSIVE、每分钟定时 PASSIVE、~500 MB 紧急 TRUNCATE；v0.5.15 起越过阈值**先 PASSIVE**，搬不完才强制 TRUNCATE；因 RESTART 曾无限期挡写（#724）而移除了 RESTART。 | passive-first；TRUNCATE 只作"超过阈值的紧急刹车"；PASSIVE 忙就跳过、记日志、不算错。 |
| [rqlite: Taking control of the SQLite WAL](https://philipotoole.com/taking-control-of-the-sqlite-wal/) / [`db/db.go`](https://github.com/rqlite/rqlite/blob/master/db/db.go) | 自己驱动检查点，TRUNCATE 前把**该连接**的 busy_timeout 临时降到 ~250ms，等不到读者就放弃并稍后重试，结束后恢复原 busy_timeout。 | 短 busy 预算 + 失败重试，而不是用默认 5s 去等。 |

## 决定

1. **DSN 加 `_pragma=journal_size_limit(134217728)`（128 MiB）**，对池中每条连接生效。
   - 稳态 WAL 只有几 MiB（autocheckpoint 1000 页 ≈ 4 MiB），128 MiB 不影响写入；
   - 不取 0：日志写入密集，每次回绕都截到 0 再扩容是白白的 I/O；
   - 不取更大：PocketBase 200 MB、Litestream 500 MB 是同量级；本库磁盘曾被挤满，取偏小值。
   - 只在 WAL 回绕时生效；长读者挡住回绕时由下面的周期任务兜底。
2. **周期 WAL 检查点任务（每 5 分钟，启动时不立即跑）**：
   - 先 `wal_checkpoint(PASSIVE)`：不等待、不持写锁，任何时候都安全；
   - 仅当 `-wal` 文件 ≥ 256 MiB（`2 × journal_size_limit`）**且** PASSIVE 已把全部帧搬回主库时，
     才在**钉住的单条连接**上把 busy_timeout 临时降为 200ms，执行 `wal_checkpoint(TRUNCATE)`；
     结束后恢复为 5000ms，恢复失败则把该连接作废（`driver.ErrBadConn`），绝不把短超时连接还回池子；
   - PASSIVE 没搬完（有读者停在旧快照上）→ 不试 TRUNCATE，标 busy，下轮再来。
     原因：TRUNCATE 会先抢写锁，再在持锁状态下搬剩余帧并等读者；剩余帧若有几 GB，
     请求写入会被按住几十秒。先 PASSIVE 搬完，TRUNCATE 只剩"等读者 + ftruncate"，
     写锁持有时间被 200ms 封顶；
   - 与 VACUUM 互斥：`dbVacuumMu.TryLock()`，抢不到就跳过本轮，不排队；
   - 只对 SQLite；MySQL/PostgreSQL 直接返回；
   - 日志：平时 Debug；真把大 WAL 截小（缩小量 ≥ 128 MiB）时 Info，便于部署后确认 9.5G 缩回。
3. **自动回收路径之后顺手截一次**：`relayLogVacuum`（清理后回收、死空间回收都经过它）在
   `sqliteVacuumLocked` 成功后，仍持有 `dbVacuumMu`，以同样的 passive-first + 200ms 预算
   强制尝试 TRUNCATE（不看阈值）。失败只记 Debug，不算回收失败。
4. **`wal_autocheckpoint` 保持默认 1000 页**：它负责稳态（PASSIVE、在提交时顺带做），
   正常；问题不在检查点频率而在"文件不缩"。
5. 手动 `DBCompactNow` 维持原样（默认 5s busy 的 TRUNCATE），管理员按钮本就预期"现在就看到变小"。

## 部署后预期

- 首个 5 分钟周期起，只要某一轮 PASSIVE 能搬完且 200ms 内没有读者占着 WAL，9.48 GB 的 `-wal`
  就会被截到 0，并打出一行 Info：`sqlite wal checkpoint (periodic): wal file 9.5 GB → 0 B`。
- 即使 TRUNCATE 一直不成功，只要 WAL 发生一次回绕，`journal_size_limit` 也会把它截到 128 MiB。
- 首次 PASSIVE 可能需要把大量帧搬回主库（I/O 较重，但不持写锁、不挡请求）。

## 风险与边界

- TRUNCATE 等待期间写者最多多等约 200ms（busy 预算），之后写者的 5s busy_timeout 继续兜底。
- 一直存在长读者（如超长全表扫描）时 TRUNCATE 会持续 busy；这是有意的——宁可晚截，不挡写。
