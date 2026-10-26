# Dustkeeper 一期实现与方案文档的合规核对

> 核对对象：`dustkeeper-phase1-implementation-20261025.md`（下称"文档"）第 2–16 节。
> 核对方式：逐节读代码 + 在本机（darwin/arm64, APFS）实跑，不以"设计上应该有"代替证据。
> 核对日期：2026-10-26。
> 判定：**符合**（行为与文档一致）／**偏离**（实现了，但与文档写法不同）／**缺失**（文档要求的行为不存在）。

## 0. 结论先说

文档 §2.3 的 15 条不变量，13 条有实现与测试证据。**2 条此前不成立，本轮已修**（§9.5 keeper 依赖、§11.4 保护别名冻结）。

但**不能按 §16 的验收清单签字**。还有 2 项 P0、5 项 P1 行为缺口，以及 §14.1 里 10 个用例（A02/A03/A04/A09/A10/A13/A19/A20/A21/A23）没有可执行断言。其中最要紧的是：

1. **磁盘压力门槛没有接入执行器**——配置项存在、进入计划摘要，但没有任何代码在低空间时阻止新隔离（违反不变量 11 与 §4.3 的明文规定）。
2. **时钟确认会一笔勾销不确定性**——`doctor --acknowledge-clock` 之后立刻恢复清空，没有把"无法证明保留期"的对象 `purge_not_before` 延后到"确认时刻 + 完整保留期"（§11.2 明确禁止这种抹平）。

## 1. 本轮已修复（7 项）

| # | 条款 | 修复前 | 修复后 | 证据 | 回归测试 |
|---|---|---|---|---|---|
| 1 | §3.1 版本信息 | `version --json` 只有 version/go/policy/platform/sqlite；构建未启用 `sqlite_omit_load_extension` | 增加 `commit`、`build_tags`、`built_at`、`driver`、`schema`；`build/build.sh` 用 `-tags sqlite_omit_load_extension` 与 `-X` 注入构建事实 | `cmd/dustkeeper/system.go:16-45`、`cmd/dustkeeper/main.go:226-246`、`build/build.sh:22-33` | `TestVersionEnvelope` 断言 9 个键 |
| 2 | §5.5 输出合同 | `message` 为空时整键缺失；计数嵌在 `counts` 对象里 | `message` 恒存在；`planned_count/completed_count/skipped_count/failed_count` 提升为顶层键 | `cmd/dustkeeper/main.go:167-190` | `TestEnvelopeHasNoNoise` |
| 3 | §12.5 resume | `resume` 只取控制锁，可与运行中的 `run` 竞争 | 先取实例执行锁，再取控制锁（与执行器同序）；拿不到就 `INSTANCE_BUSY` 退出 3，暂停标记保持 | `cmd/dustkeeper/commands.go:cmdResume` | `TestResumeNeedsTheExecutionLock` |
| 4 | §2.1 assisted 前提 | 只有重复计划要求 `--writers-stopped`，`assisted` scope 的普通计划可以直接执行 | 任何落在 `assisted` scope 的计划都要求该声明 | `cmd/dustkeeper/cmd_plan.go:239-250` | 手工实跑：无标志 → `INVALID_ARGUMENT`；带标志 → `1 of 1 completed` |
| 5 | §9.5 keeper 依赖 | `Store.KeeperInUse` 定义完整但**没有任何调用者**；生命周期规则可以把仍在被隔离对象依赖的 keeper 移走 | 隔离前查询活跃 keeper 依赖，命中则 `SKIPPED_PROTECTED` 并说明原因 | `internal/execute/quarantine.go:121-131` | `TestTheKeeperOfAnIsolatedDuplicateIsNotQuarantined` |
| 6 | §11.4 保护别名 | `PROTECTION_ALIAS_UNRESOLVED` 只存在于错误码表，从不产生；原父目录不可解析时直接放行清空 | 两级判定：字面槽位命中即拦；再按内核身份比对父目录与条目。原目录消失/被替换时返回 `PROTECTION_ALIAS_UNRESOLVED` 冻结，不猜 | `internal/execute/purge.go:298-410` | `TestPurgeFreezesWhenAliasCannotBeResolved` |
| 7 | §10.4 时间可信 | `plan apply` 对清空计划硬编码 `clockTrusted=true`，手工清空可绕过时钟检查 | 手工清空与自动清空走同一套时钟判定，不可信则 `CLOCK_UNTRUSTED`，零删除 | `internal/run/runner.go:ClockTrust`、`cmd/dustkeeper/cmd_plan.go:268-278` | 现有 `TestClockGoingBackwardsSuspendsPurge` 覆盖自动路径 |

修复后全量结果：

```
go vet ./...                  干净
go test ./... -count=1        23 个包全绿（集成用例 23 个）
go test -race ./... -count=1  全绿
```

## 2. 仍未满足（按严重度）

### P0-1 §4.3 / §10.6 / 不变量 11：磁盘压力门槛未接入

`min_free_bytes` 与 `min_free_percent` 出现在 `internal/config/config.go:46-47`、通过 `plan/binding.go:199-200` 进入 `ConfigDigest`，也被塞进 `execute.ConfigView`（`internal/execute/quarantine.go:39-40`），但**没有任何一行代码读取它们**。`CodeDiskPressure` 同样从未产生。

文档原文：「空闲空间门槛按"低于字节阈值或百分比阈值"触发暂停新隔离」「隔离区达到容量门槛 → 暂停新隔离，保留未到期对象」。

影响：磁盘将满时仍会继续隔离。同卷 rename 不释放空间，所以这不会直接把盘写满，但会让"隔离区在快满的卷上继续长大"，并且违反文档里唯一一条关于资源门槛的强制条款。`docs/architecture.md` 把它写成"已知限制"，但文档 §4.3 是规定而不是建议。

修法（约 40 行）：`execute.Quarantine` 在写 manifest 之前 `unix.Statfs` 判 `Bavail*Bsize` 与百分比，低于门槛返回 `DISK_PRESSURE`（退出 7），不移动；`run.runScope` 在 `quarantineGrants` 之前判一次，低于门槛整体跳过隔离但仍继续清空（文档：已到期、已获授权的正常清空可以继续）。

### P0-2 §11.2：时钟确认抹掉了不确定性

`internal/run/runner.go:376-398` 的 `clockTrust` 实现三个阈值（回退 5 分钟、跳跃 48 小时、运行内漂移 5 分钟），这部分正确。问题在确认动作：

- `cmd/dustkeeper/commands.go:203` 只写 `clock_ack_ns = now`。
- `clockTrust` 在 `ack > last` 时直接判定可信，随后 `purgeDue` 立即按原 `purge_not_before_ns` 执行。

文档要求：「`doctor clock acknowledge` 必须显示受影响对象；恢复自动清空时，将无法证明保留期的对象 `purge_not_before` 延后到至少"确认时刻 + 完整保留期"，不能借确认动作把不确定时间一笔勾销。」

现状等于：停机 60 天后跑一次 `doctor --acknowledge-clock`，所有在停机期间"名义到期"的对象立刻可删——而那 60 天从未被观察到。

修法：ack 时把 `last_run_ns` 到 `ack_ns` 之间到期的 `items` 全部重算 `purge_not_before_ns = ack_ns + bound_retention_seconds*1e9`，写一条 `PhaseInfo` 事件记录受影响对象数与原因；`doctor` 输出该列表。

### P1-1 §8.3 / §10.6：没有只读清单入口

文档：「数据库损坏时，`recover inspect --manifests` 只能生成只读恢复清单」。当前 `recover inspect` 先 `openStore()`（`cmd/dustkeeper/cmd_object.go:590-593`），数据库打不开就整体失败；全仓没有读取 `.dustkeeper/manifests/*.json` 的代码（只有 `recovery/reconcile.go:405` 在数据库可用时读单个清单）。

所以「DB 损坏 → 冻结变更 + 只读清单」这条路径不存在。这是 A10 用例的核心断言。

### P1-2 §9.1 / §12.2：`DEEP_SCAN_REQUIRED` 从不产生

错误码在 `errs.go:79` 定义，`errs_test.go` 断言了它的退出码，但没有任何代码路径会产生它。文档要求「普通运行连续三次因同一大文件或超大目录无法完成时，标记 deep_required 并跳过该项继续其他工作，避免队列饥饿」。当前 `scan_queue` 没有失败计数列，`ClaimDirs` 也没有跳过逻辑。

### P1-3 §3.1 / §4.3 / §13.2：没有日志子系统

全仓 `grep slog` 为零。`config.Paths.LogDir` 被创建（`cmd/dustkeeper/commands.go:32`）、被 `doctor` 检查、被 launchd 模板写成 `StandardOutPath`，但**没有任何一行代码往里写**。

缺失的是三件文档明确规定的事：

- 运行日志按 5 MiB × 5 轮转（§13.2）；
- JSONL 导出与"导出失败只报告，权威审计不能缺失"（§10.6）；
- 路径与秘密信息脱敏的结构化日志（§3.1「日志：`log/slog` + 数据库事件，限制路径与秘密信息泄露」）。

后果：后台任务失败时，`launchd` 指到一个永远空的文件，`systemd` 只能靠 stdout；`doctor` 报告"logs 目录存在"却没有任何内容可看。

### P1-4 §8.2：store 未校验 `auto_purge_grant_id` 的来源

文档：「`auto_purge_grant_id` 外键存在还不够：必须是 active 的 auto_purge，scope/规则版本/所有绑定与 scheduled_rule 来源完全一致。manual_rule/manual_duplicate 的该字段必须为空，由 store 检查。」

现状：`store.InsertItem`（`internal/store/items.go:130-150`）只写列，不校验；全部校验集中在 `execute.checkAutomaticAuthority`（`internal/execute/purge.go:244-296`），执行时确实拦住了。也就是说**安全属性成立，但文档指定的防线位置没有**——一张手工计划如果被写入 grant 引用，只有在清空那一刻才会暴露。

### P1-5 §8.4 / §10.2：补偿恢复的合同不是强类型，实际捕获身份未入库

`internal/execute/quarantine.go:420-432`：

```go
ContractJSON: `{"purpose":"miscapture_compensation"}`,
```

文档 §8.4 要求 `OperationContractV1` 至少含 item ID、`execution_plan_id`、`prior_state`、purpose、源/目标精确路径与父目录身份、预期对象身份、复验证据、当前绑定与预算；§8.2 还要求「实际捕获身份」进独立列（`captured_identity_json`）且不得覆盖 `pre_identity_json`。

现状：实际身份只出现在 hold 事件的 payload 里（`miscapture` 的 `captured_identity` 字段），`items.captured_identity_json` 保持为空；补偿恢复的合同是一段自由 JSON。对账与人工审计要解析事件字符串才能知道"到底移走了什么"。

### P1-6 §10.2 第 8 步：移动后只复核了内容，没有复核元数据

`quarantine.go:286-297` 在 rename 后重新算内容哈希并 `SyncAfterMove`，但没有重新 `MetadataOf(obj)` 比对 `metaHash`。文档原文：「核对实际移动到 vault 的对象 identity；**重新完整校验内容与必要元数据**，建立 captured token」。`Purge` 一侧做了元数据复核（`purge.go:115-127`），隔离一侧漏了。

### P1-7 §10.4：keeper 复验的 I/O 没有计入预算

`purge.go:129` 只用 payload 大小检查 `RemainingRead()`，`VerifyKeeper`（第 143-150 行）会完整读一遍 keeper 文件，这部分读取不进预算。文档要求「在任何完整读取之前为 payload/keeper 的全部复验预留 I/O 与动作额度；不足时不开始本项」。当前实现会出现"payload 读完了才发现 keeper 读不完"。

### P2 结构与覆盖

| 项 | 文档要求 | 现状 |
|---|---|---|
| `internal/report` | §7.1 列出该包，T01 要求 `go test ./internal/report` | 不存在，输出逻辑在 `cmd/dustkeeper` |
| `internal/testkit` | §7.1、T06 要求假时钟与故障注入 | 不存在；假时钟在 `test/integration` 内部自建，`internal/execute` 无法注入故障 |
| 包内单测 | T04/T06/T07/T08/T10 各自指定测试命令 | `internal/scan`、`execute`、`recovery`、`dedup`、`run` **测试文件数为 0**，全部靠 23 个集成用例覆盖主路径 |
| 字节统计 | §13.1 精确区分逻辑大小 | `runner.go:220-225`、`338-344` 按 `Results` 下标对 `Items` 取值，隐含"结果顺序与条目顺序一致"的假设；一旦执行器改变追加顺序就会错报字节数 |
| Go 版本 | §3.1 初始工具链 Go 1.27.3 | `go.mod` 声明 1.26.1（本机唯一可用工具链）。属于环境约束，发布前需按 §3.1 重新锁定 |

## 3. §14.1 验收用例覆盖矩阵

| 编号 | 用例 | 覆盖情况 | 证据 |
|---|---|---|---|
| A01 | 未注册路径、越界、链接、嵌套挂载 | ⚠️ 部分 | `TestSymlinkIsNotFollowed`、`TestCrossDeviceIsRefused`、`scope` 注册测试；**没有**"未注册路径直接进执行器"的断言 |
| A02 | 检查与 rename 之间替换叶子 | ❌ 缺失 | 需要 `internal/execute` 故障注入点，当前无法构造 |
| A03 | 隔离后原写 FD 继续修改 | ❌ 缺失 | `checkNoWriterChange` 存在但无测试 |
| A04 | 每个 intent/rename/unlink/sync/commit 边界中断 | ⚠️ 1/12 | 只有 `TestReconcileCompletesACrashedQuarantine` 一个边界 |
| A05 | 恢复目标已存在或并发出现 | ✅ | `TestRestoreRefusesToOverwrite` |
| A06 | 大小写/Unicode 等价目标与保护路径 | ✅ 部分 | `TestProtectionUsesIdentityNotSpelling`（APFS 大小写）；Unicode 归一化未测 |
| A07 | keeper 删除/替换/内容变化 | ⚠️ 1/3 | `TestPurgeRefusesWhenTheKeeperIsGone` 只测了删除 |
| A08 | 假哈希碰撞、元数据不同 | ⚠️ 手工 | 无自动用例；`CompareFiles` 在哈希相等后逐字节比较，逻辑正确但无回归保护 |
| A09 | ENOSPC/IOERR 在 intent 前后 | ❌ 缺失 | — |
| A10 | DB 损坏、孤儿清单、未知 payload | ❌ 缺失 | 与 P1-1 同源 |
| A11 | 两 state 同 scope、父子 scope、根/卷替换 | ✅ | `TestSecondInstanceIsRefused`、`TestTheSameTreeCannotBeRegisteredTwice`、`TestNestedScopesAreRefused`、`TestOpenVerifiedDetectsAReplacedRoot` |
| A12 | 到期但无 auto_purge、后补、撤销 | ⚠️ 2/3 | `TestAutoPurgeIsNotRetroactive`；撤销后冻结无断言 |
| A13 | 正确双授权 + 到期 → 后台确实清空 | ❌ 缺失 | 产品主张能力，只有手工 CLI 实跑记录 |
| A14 | 时钟回退、跳跃、长时间停机 | ⚠️ 1/3 | `TestClockGoingBackwardsSuspendsPurge`；跳跃与停机无测试 |
| A15 | 恢复后以新 inode 重写原路径 | ✅ | `TestRestoreProtectsAndBlocksTheNextPlan` |
| A16 | 模型返回 delete/shell/未知/重复字段 | ✅ | `compiler_test.go` 12 类拒绝、`strictjson` 10 类拒绝 |
| A17 | "没用过"、否定、例外、多阈值 | ✅ | `zh_corpus.json` 47 条 + `corpus_test.go` |
| A18 | 计划过期、版本/保护/配置改变 | ✅ | `TestExpiredPlanIsRefused`、`TestWrongDigestIsRefused` |
| A19 | 手工与后台并发 | ❌ 缺失 | 实例锁有测试，但"手工与后台同时操作同一 scope"没有 |
| A20 | run/restore/purge 断网 | ❌ 缺失 | 无"模型请求计数为零"的断言 |
| A21 | 新垃圾与已到期对象并存 | ❌ 缺失 | 公平性与 50% 预算分配无测试 |
| A22 | 隔离后按原路径及 APFS 别名新增保护 | ✅ 本轮补 | `TestPurgeFreezesWhenAliasCannotBeResolved` + 字面槽位分支 |
| A23 | run 持锁时 pause/revoke | ⚠️ 部分 | `TestPauseStopsNewIntents`、`TestResumeNeedsTheExecutionLock`；重启后仍暂停未断言 |

统计：**完全覆盖 7 项，部分 7 项，缺失 9 项**。§14.1 的表头是"必须阻断发布的测试"，因此这 9 项在文档意义上是发布阻断项。

## 4. 核对通过、且有实测证据的部分

这部分不需要改动，列出是为了说明"哪里确实对得上"，也便于复审抽查。

| 条款 | 证据 |
|---|---|
| §8.1 PRAGMA 全量与读回 | `internal/store/store.go:87-128`，含 `journal_mode=DELETE`、`synchronous=EXTRA`、`busy_timeout=1000`、`cache_size=-8192`、`mmap_size=0`、`temp_store=FILE`、darwin `fullfsync=ON`；`TestSchemaAndPragmas` 逐项回读 |
| §8.2 DDL 基线 | `migrations/001.sql`：11 张表全部 `STRICT`，`items.state`、`plans.status`、`grants.capability` 等 CHECK 与文档一致，9 条索引齐全（另有 2 条查询索引） |
| §6.6 摘要测试向量 | `internal/rules/schema_test.go:10-38` 断言规范字节与 `sha256:a1128b67…f4d6` 双向一致 |
| §6.3 字段合同 | `rules/schema.go:114-180`：年龄 86400–315360000、大小 0–9007199254740991、前缀 32/64、模式 32/64、保留期 604800–31536000 且必须整天 |
| §6.5 计划有效期 | `plan.DefaultTTL = 15 * time.Minute`（`planner.go:19`），逐项 `validateItem` 再验一次 |
| §7.4 / §10.1 不覆盖原语 | Linux `openat2` + 四标志（`platform_linux.go:36`）、`renameat2(RENAME_NOREPLACE)`（`:81`）；darwin 逐段 `O_NOFOLLOW`（`platform_darwin.go:24`）、`renameatx_np(RENAME_EXCL)`（`:57`）；无 copy+unlink 兜底 |
| §10.2 Q 顺序 | `quarantine.go:80-328`：核验 → 预算预留（×2 读）→ 哈希 → manifest 持久化 → intent 提交 → rename → 移后核验 → 同步 → 提交，与文档十步一致 |
| §12.5 暂停临界区 | `scope/locks.go:195-216`：控制锁内判标记并提交 intent，顺序恒为实例锁 → scope 锁 → 控制锁 |
| §12.1/§12.2 预算 | `runner.go:312-315` 取 `min(配置, grant)`；`:177-179` 零额度视为禁止；`:209` 清空只取本轮预算的 1/2 |
| §12.1 轮转 | `runner.go:402-435`，游标持久化在 `meta`，每次前进一格 |
| §9.4 元数据等价 | `fsops/metadata_{darwin,linux}.go`：单值 1 MiB、总量 4 MiB；ACL 用属性存在性检测，存在即 `UNSUPPORTED_METADATA` |
| §9.1 扫描 | `scan/scanner.go:59-61`（批 500、`ReadDir(128)`）、`:203-207`（`nlink>1` 跳过）、`:254-261`（只在完整 epoch 后标 stale） |
| §13.1 空间口径 | `run.Report`：`quarantined_logical_bytes`、`purged_logical_bytes`、`disk_free_bytes_before/after`，不提供 `actual_reclaimed_bytes` |

## 5. 建议的收尾顺序

1. **P0-1 磁盘门槛**、**P0-2 时钟确认**——两处都直接对应文档的强制条款，改动小、可测。
2. **A13 + A21**：写一个真正的"双授权 → 到期 → 后台清空"集成用例，顺带覆盖公平性与 50% 预算。这是产品唯一的主张能力，目前只有手工记录。
3. **A04 崩溃矩阵 + A02/A03**：需要先补 `internal/testkit/faults.go`（可注入 rename/sync/commit 失败），否则 §10.6 的失败点表格无法验证。
4. **P1-1 只读清单**、**P1-3 日志轮转**：运维可见性的两条腿。
5. **P1-4/P1-5/P1-6/P1-7**：收紧 store 防线与证据链，成本低但对审计友好。
6. **包内单测**（scan/execute/recovery/dedup/run）与 `internal/report`、`internal/testkit` 的落位。

## 6. 一句话判断

实现的主干——排他 rename、先记账后动手、不覆盖恢复、对账矩阵、预算取最小值、摘要绑定——是**按文档写的**，不是"看起来像"。差距集中在两类：**资源与时间这两条全局约束还没有落到执行器**（P0），以及**故障注入类的证据链几乎为空**（A02/A03/A04/A09/A10）。在这两项补齐之前，§16 的验收清单不能勾完。
