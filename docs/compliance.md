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

---

# 第二轮复核（`dustkeeper-code-review-and-next-plan-20261026.md`）整改对照

> 核对方式：先写能在旧版证明问题的失败测试，再修；结论只跟测试和实跑走。
> 全量结果（2026-10-27，darwin/arm64, APFS + Linux 容器）：
>
> ```
> go build ./...                干净
> go vet ./...                  干净
> go test ./... -count=1        全部通过（含 test/bench 跳过、test/release、test/eval）
> go test -race ./... -count=1  全部通过
> ./build/test-linux.sh         全部通过（Docker golang:1.26-bookworm + ext4 loop，privileged）
> ./build/build.sh              dev：darwin-arm64 产出 + SHA256SUMS + build-facts.txt，smoke=ok
> ./build/build.sh --release    缺三个交叉目标 → 退出 1
> bench 10 万 / 100 万          docs/evidence/performance-{100k,1m}.md（由运行生成）
> ```

## 7.1 逐项状态

图例：**关闭**＝有失败测试复现问题、修复后通过；**实现待补测**＝代码已改、全量测试通过，但还没有能独立复现原问题的用例；**未做**。

| 编号 | 主题 | 状态 | 证据（代码 / 用例） |
|---|---|---|---|
| R01 | 私有管理目录可信边界 | 关闭 | `fsops/private.go`（`PrivateExpect`、`InstallPrivate`、`VerifyPrivate`、`ReadFileAt`）、`scope/vault.go`；`TestRegistrationNeverFollowsAPlacedTempSymlink`、`TestRegistrationRefusesAnAnchorHardLink`、`TestCorruptOwnerMarkerIsRefused`、`TestWidenedVaultPermissionsAreRefused`、`TestReplacedObjectsDirectoryIsRefused`、`TestVaultWithoutRecordedIdentityIsRefused`、`TestProbeReportsRequiredCapabilities` |
| R02 | scope 锁与根身份 | 关闭 | `scope/locks.go`（in-tree `scope.lock` + 进程内引用计数）、`scope/identity.go`（`CompareRootIdentity` / `CompareMountBinding`）、`run/runner.go` 对 `IdentityWeak` 禁用自动清空；`TestHeldScopeLockBlocksTheNextTask`（真子进程）、`TestSameProcessReopensShareTheLock`、`TestEveryStrongFieldCanRefuseTheObject`、`TestStoredRootOwnerZeroIsCompared`、`TestNewBootIsNotAChange`、`TestRemountWithinOneBootIsAChange` |
| R03 | 保护在所有入口一致执行 | 实现待补测 | `policy.Guard` 被 `scan`、`plan`、`execute/quarantine`、`run` 共同引用（`CheckExact/CheckSubtree/CheckFile`）；`TestGlobalProtectionWinsOverTheRule`、`TestProtectionUsesIdentityNotSpelling`、`TestPurgeFreezesWhenAliasCannotBeResolved`。**缺**：scan 后新增 subtree、scope 根是 Git 仓库、`.git` 为 worktree 文件这三类入口场景 |
| R04 | 命令入口分叉与统一控制 | 实现待补测 | `domain.Control{StopScope,StopRun}` + `execute/apply.go` 的 `strongerControl`，两条 apply 路由读同一成员；`TestPauseStopsNewIntents`、`TestResumeNeedsTheExecutionLock`。**缺**：plan 后 pause、第一规则失败后第二规则无 intent 的断言 |
| R05 | 清空时钟边界 | 实现待补测 | `internal/clocktrust`（`last_trusted_run_ns` 与当前壁钟分离、`Acknowledge` 重写受影响对象的 `purge_not_before`）、`cmd_plan.go` 与 runner 共用同一判定；`TestClockGoingBackwardsSuspendsPurge`。**缺**：跳钟后连续两轮仍不可信、确认后必须保留完整新窗口 |
| R06 | ParkIntent 与终结事件冲突 | 实现待补测 | `domain.PhasePark` 明确定义为非终结观察，`store.NoteParked` 保留 pending 与原合同，终结只走合法状态转移；`TestCompleteQuarantineIsIdempotent`。**缺**：Q/R/P 各自「系统调用成功后注入同步失败 → 对账两轮」的矩阵 |
| R07 | 错误捕获对象的可恢复性 | 实现待补测 | `captured_identity_json` / `captured_token_json` 与 hold 一致提交（`execute/quarantine.go`、`store/items.go`），`restore.go`/`purge.go` 读捕获证据，miscapture 复用 `PurposeMiscaptureFixup` 合同并停在 held；`TestReconcileCompletesACrashedQuarantine`。**缺**：原路径被新文件占用、补偿 intent 后崩溃 |
| R08 | 崩溃对账的证据与持久化 | 关闭 | `plan/contract.go`（严格合同 + 必填字段）、`recovery/reconcile.go`（manifest 缺失/损坏/身份不一致一律拒绝，`ReadStable` 复核，`SyncAfterMove`/`SyncAfterUnlink`）；`TestReconcileRefusesAMissingManifest`、`TestReconcileRefusesACorruptManifest`、`TestReconcileRefusesDisagreeingEvidence`、`TestReconcileRefusesAContractWithoutEvidence`、`TestReconciledQuarantineStillReachesPurge` |
| R09 | 精确重复与清空的证据链 | 关闭 | `dedup/dedup.go`：元数据摘要真实计算并写入计划、`VerifyKeeper` 核 nlink/权限/归属/mtime/元数据/逐字节、reference keeper 替换同样核验、一对一同范围/跨范围重复都能成组；`TestDuplicateIsHeldWhenTheKeeperPermissionsChange`（及 Metadata/Link/Bytes 三个变体）、`TestDuplicateIsPurgedWhenTheKeeperIsIntact`、`TestOneToOneCrossScopeDuplicateIsFound` |
| R10 | 后台自动清空被零额度拦住 | 实现待补测 | `domain.ActionLedger` 分为 Q/P 两个独立账本，`runner.go` 用 `MaxAutoPurgeFiles/Bytes` 建 P 账本；`TestZeroBudgetForbidsTheAction`、`TestBudgetExhaustionLeavesTheRestAlone`。**缺**：双授权 → 到期 → 后台确实清空的端到端断言 |
| R11 | 扫描中断被当成完成 | 关闭（A19/A20） | `scan_queue` 增加 `failed`、`attempts`、`claim_seq`、`last_error`（migration 003），`scopes.scan_interrupts` 记录连续未完成轮次。失败的目录不再标 done：它留在队列里，`ScanQueueStateOf` 把它计入 pending，所以既不能完成 epoch，也不能退休自己的 observation。领取按 least-recently-claimed 排序，中断时把同批未被打开的目录 `ReleaseClaimed` 回 pending（`claim_seq=NULL`），超大目录因此不能每轮重新抢在最前。用例：`TestAFailedDirectoryCannotCompleteTheEpochOrRetireItsOwnObservations`（A19）、`TestAnInterruptedDirectoryDoesNotStarveTheOnesBehindIt`（A20）。顺带修掉 `OpenDir("")` 用 dup 共享文件偏移、第二次列举根目录返回空列表的缺陷（`fsops.Root.openRoot`，重开并核对 dev/ino）。**缺**：`internal/scan` 仍无包内单元测试，证据都在 `test/integration` |
| R12 | 自然语言能力与安全落地 | 关闭（5 项里 5 项；A22 真实模型报告仍缺） | 见 §7.2；`TestEveryDocumentedFieldIsRequired`、`TestRequiredFieldsRejectNull`、`TestEmptyCollectionsAreNotMissingFields`、`TestFieldsMustHaveTheDocumentedShape`、`TestMaxSizeBytesMustBePresentButMayBeNull`、`TestParseDraftRejectsCaseVariantsOfItsOwnKeys`、`TestParseRejectsTwoSpellingsOfOneField`、`TestParseAcceptsTheExactFieldNames`、`TestModelDraftsGoThroughTheSameFieldContract`、`TestDraftVariantsAreJudgedByFieldPresence`、`TestDraftRequestCarriesTheAllowedScopes`、`TestDraftRequestBodyMatchesTheSnapshot`、`TestNormalCommandsNeverCallTheModel`、`test/eval`（`TestTheSemanticGateCatchesWhatTheReviewNames` 等） |
| R13 | 脱离数据库的救援入口与运行诊断 | 关闭（P1 + P2，A23 覆盖） | **不可读≠不存在**：`rescue.Inspect` 现在按目录状态给结论——`object_state=read` 才能说"某个对象不在"；`missing`（整个 objects 目录 ENOENT）是对全体的确证；`unreadable` 只产出 `unverified`（原来会产出 `manifest_without_object`，把"没看成"说成"没了"）。单条目 stat 失败也从 `conflict` 改判 `unverified`。CLI 输出 `objects directory: <state>` 与 `unverified` 计数，`unverified>0` 时退出 6 并给出 `next_steps`。**诊断**：`internal/diag` 写 `<state>/logs/dustkeeper.jsonl`（2 MiB × 3 轮转、单字段 4 KiB 截断、0600、UTC RFC3339Nano），`phase=begin|result|run|scope|op` 五类记录，字段含 run_id / command / instance / scope_id / plan_id / object_id / op_id / item_id / action / outcome / code / status / exit / retryable / control / next_steps / message 与本轮读取字节、动作、待处理计数；`recovery.Result` 增加 `op_id` 并逐条入日志。用例：`TestAnUnreadableObjectsDirectoryDoesNotClaimTheObjectsAreGone`、`TestAMissingObjectsDirectoryIsReportedAsMissingNotUnverified`、`TestRecoverInspectReportsAnUnreadableObjectsDirectory`、`TestRecoverInspectOnAReadableDirectorySucceeds`、`TestTheLogRecordsTheRunAndItsConclusion`、`TestTheLogNeverCarriesTheModelKeyOrFileContents`、`TestEveryRecordUsesTheDocumentedKeys`、`TestARecordCarriesTheDiagnosticFields`、`TestTheLogRotatesAndStaysBounded`、`TestAnOversizedFieldIsTruncatedNotDropped`、`TestANilLoggerIsSafe`、`TestTheLogFileIsNotWorldReadable`。**仍缺**：执行期 Q/P intent 的 op_id 没有逐条进日志（只有对账结果有），`internal/rescue` 无包内单测 |
| R14 | 性能证据 | 部分关闭（扫描有界已证，去重内存未关） | 证据文件由运行生成，不手写：`docs/evidence/performance-100k.md`（10 万、30% 重复）与 `docs/evidence/performance-1m.md`（100 万、0% 重复），各带 JSON sidecar 与树形/大小分布/fan-out。10 万（30% 重复）：扫描 100 000 文件 / 1 051 目录、wall 38.5 s（CPU 1.7+3.4 s），epoch 完整、队列 pending 0、observation 行数 100 000、峰值 RSS 30.0 MiB、库 59.0 MiB（619 B/文件），逻辑重复字节 52.3 MiB；中断续跑 8 批累计扫描 1.00× 树大小、最长单批 5.2 s、各批峰值 31.0 MiB。100 万（0% 重复）：扫描 1 000 000 文件、wall 79.9 s（CPU 6.9+14.6 s）、峰值 RSS 30.8 MiB、库 588.5 MiB（617 B/文件）；续跑 17 批、1.00×、最长单批 7.8 s。**未关闭**：`dedup` 峰值 RSS 389 MiB（4.1 KB/文件；100 万、0% 重复时 1 158 MiB / 1.2 KB/文件）随文件数线性增长，超出 256 MiB 扫描门槛——已改为按大小分组流式处理（不再同时持有 bySize/byHash/结果三份索引），但结果集本身仍是每重复组两个候选，需分页/逐组落盘才能达标。顺带修掉一处一直错报的数字：`dedup.Result.LogicalBytes` 声明了、CLI 也在报，但从未赋值，所以每个重复计划都写 `logical_bytes: 0`——"这个计划能省多少"被回答成 0。扫描 wall 时间也被**每目录一次提交**主导（2 万文件 31.5 s、10 万文件 38.5 s，目录数同为 1 051，约 30–37 ms/目录；`synchronous=EXTRA` 下每目录 claim 与 done 各一次提交/fsync，文件数从 2 万涨到 10 万只多 7 s）。这两条按 §6.4-10 记为未关闭 |
| R15 | 发布脚本与平台声明 | 关闭（真实 musl + 产物检查 + 冒烟） | `build/build.sh` 对每个 linux 目标做**双重证明**：编译器 `-v` 输出里必须有 musl 证据（`musl-gcc.specs` / musl 头路径），制品再用 `build/verify-artifact`（`debug/elf`：Machine、ET_EXEC/ET_DYN、PT_INTERP、`ImportedLibraries`）判 `linkage=static`；缺任一证据即退出 1，`ALLOW_UNPROVED_LIBC=1` 只允许 dev 记 `linkage=unproved` 且不写发布 manifest。`build-facts.txt` 逐目标记录 `format/arch/linkage/toolchain/compiler_version/sysroot/musl_evidence/build_env/artifact/smoke`，发布 manifest 由 facts 生成并在写盘前校验每个 linux 目标都是 `linkage=static`。本机冒烟用 `<binary> version`（不是 `--version`），非本机目标记 `smoke=not-runnable-here`。容器内真实构建 linux/arm64 静态制品：`readelf` 事实 `interp=none needed=0`，`smoke=ok`。用例：`TestAPureGoLinuxBinaryIsRecognisedAsStatic`、`TestTheWrongArchitectureIsRefused`、`TestATextFileIsNotAnArtifact`、`TestTheDarwinPathReadsARealMachO`、`TestReleaseFailsWhenTheLinuxToolchainIsNotMusl`、`TestDevelopmentBuildRefusesToChecksumAnUnprovedArtifact`、`TestReleaseFailsWhenTheArtifactIsNotStatic`、`TestADeveloperCanBuildAnUnprovedLinuxArtifactExplicitly`、`TestReleaseSucceedsWhenEverythingIsPresent`；`--label` 贯通与显式 00:00 由 `TestScheduleRenderHonoursMidnightAndLabel`、`TestScheduleRenderKeepsTheDocumentedDefault`、`TestLabelDrivesTheFileNames`、`TestExplicitMidnightIsHonoured` 覆盖。**仍缺**：linux/amd64、darwin/amd64 未在真机执行，只记 `not-runnable-here` |

## 7.2 R12 的五项整改

1. **必填字段与 absent/null/空集区分**：`strictjson.Inspect` 在 token 层记录每个对象成员的路径与形状（`presence.go`），`rules.ruleFields` 声明 12 个必填字段及其允许形状。`max_size_bytes` 必须出现、值可以是 `null` 或合法整数：上一版把它放进 `optionalFields`，等于把"模型漏写上限"接受成"没有上限"，违反"遗漏不得悄悄扩大匹配范围"的宿主合同。缺失现在是 `INVALID_ARGUMENT`，显式 `null` 才是"无上界"（`TestMaxSizeBytesMustBePresentButMayBeNull`）。
   **同一个对象只认一套精确键名**：`strictjson.Allow` 按每个对象的精确键集拒绝大小写变体和别名重复（`Questions`/`SCOPE_ID` 这类能被 encoding/json 直接绑定的写法），`rule` 对象与 draft envelope 各有一套键集，import 与 draft 走同一个 `checkRuleFields`，所以两条路径不会一个严一个松（`TestParseDraftRejectsCaseVariantsOfItsOwnKeys`、`TestParseRejectsTwoSpellingsOfOneField`、`TestParseAcceptsTheExactFieldNames`）。
2. **draft/needs_input/unsupported 按字段存在性判别**：`forbidFields` 让三种变体互斥到"字段出现即违规"，`questions: []`、`reason: ""`、`dsl: null` 都不再被当成没写。模型草稿的 `dsl` 以 `json.RawMessage` 交给 `rules.Parse`，与 `rule import` 走同一个校验器——测试 `TestModelDraftsGoThroughTheSameFieldContract` 用"删掉 except"这一条证明两条路径同源。
3. **把允许的 scope 列表传给 provider**：`systemWithScopes` 把排序后的逻辑 scope ID 追加到系统提示；超过 64 个时拒绝请求而不是截断（截断等于让模型从看不见的集合里选）。
4. **HTTP 请求体快照**：`internal/rules/testdata/draft_request.golden`，测试对请求体做规范化 JSON 比对，并单独断言"只有两段消息、用户消息等于原文、不含 `/srv/`、文件名、哈希"。
5. **语料解析测试与真实模型评测分离**：`internal/rules/corpus_test.go` 只验证"这些答案能被 DSL 表达、解析器接受"；`test/eval` 才调用真实模型，并把 provider、model、prompt digest、输入、原始输出、宿主决定（含"被宿主拒绝"）逐条写成 JSONL。没有 `DUSTKEEPER_EVAL_BASE_URL` 时它 `t.Skip`，网络调用不是单测前提。

另外发现并修掉一个文档没列、但让 R12 整体无法触达的缺陷：`config` 校验的 provider 名是 `chat_completions_json`，而适配器要求 `openai-compatible`，两个字符串不可能同时满足——**任何通过校验的配置都构造不出 provider**，`rule draft` 在真实二进制上直接 `INVALID_ARGUMENT: provider ... is not supported`。现在两处共用一个常量 `config.ProviderChatCompletionsJSON`，`TestTheDefaultConfigCanConstructAProvider` 用默认配置构造适配器。

**R12 最低验收里还没做到的一条**：「两个年龄阈值」「不是所有」这类语义误解不得直接形成已激活规则。宿主能保证的是"缺字段即拒绝"，不能保证模型没有把双阈值悄悄写成单阈值。为此 `test/eval` 的判定从"kind 相同"改成逐项语义比较（`test/eval/semantic.go`：scope、年龄阈值、大小上下界、match、except、保留期，外加"该澄清时是否澄清、该拒绝时是否拒绝"），并且报告路径 `DUSTKEEPER_EVAL_OUT` 现在是必填——写进 `t.TempDir()` 的报告随进程消失，等于没有证据。`TestTheSemanticGateCatchesWhatTheReviewNames` 用"90 天写成 30 天""丢掉 except""换一个仍合法的 scope""改保留期""凭空加上界""收窄模式"六种偏差证明这个门禁会失败。**但仍然没有真实模型跑过的报告**：这一条只能靠真实 provider 的输出，本轮环境没有可用的端点，因此 A22 记为未交付，不能算关闭。

## 7.3 仍未关闭

- **R14 去重内存**：按大小分组流式处理后仍是"每重复组两个候选"常驻内存，10 万文件／30% 重复实测峰值
  442 MiB（4.4 KB/文件），未达 256 MiB。要达标必须把分组结果分页落盘（SQLite 索引或逐组写计划），
  本轮只做到"不再同时持有三份全量索引"，因此 R14 记为部分关闭。
- **R14 扫描吞吐**：扫描 wall 时间由每目录一次提交主导（2 万与 10 万文件都约 32 s，1 051 个目录），
  `synchronous=EXTRA` 下每个目录 claim 与 done 各一次 fsync。功能上正确（epoch 完整性未受影响），
  但对"很多小目录"的树是明显浪费；批量化提交需要重新论证崩溃边界，本轮未动。
- **A22 真实模型评测报告**：门禁与报告格式已就位，缺真实 provider 的一次运行与逐条复核标记。
- **A06/A07（R04 残余）**：`plan apply` 与 `purge apply` 现在共用 `plan.LoadMembers`，purge 计划成员
  加载有服务层用例（`TestAPurgePlanCanBeLoadedForApproval`），但"同一 purge 计划从两条 CLI apply 结果
  一致"与"第一规则 held 后第二规则无新 intent、扫描状态写失败后其他 scope 无新 intent"仍**没有**可执行
  断言：CLI 走系统时钟，而隔离保留期只能在受控时钟下推进，端到端构造需要把 fake clock 注进 CLI。
  在两平台 CLI 层补齐之前，R04 保持"实现待补测"。
- **R03/R05/R06/R07/R10 的专门失败测试**：代码已改、全量测试通过，但缺"能在旧版复现问题"的独立用例。
  按 §6.4-1 的标准，它们只能算"实现待补测"，不能算关闭。
- **真实平台闭环**：只有 darwin/arm64 跑过"注册 → 计划 → 隔离 → 不覆盖恢复 → 到期清空 → 重启对账"。
  linux-amd64、linux-arm64、darwin-amd64 只做过交叉编译与产物事实检查，没有在目标机器上执行过，
  `build-facts.txt` 里如实记 `smoke=not-runnable-here`，文档与 README 按"实验性"标注。
- **`internal/rescue`、`scan`、`execute`、`recovery`、`run` 仍无包内单测**：`internal/diag` 与
  `internal/rescue` 的行为证据都在 `test/integration` 与 `cmd/dustkeeper`，这是刻意的（测真实入口），
  但 §7.1 的 T04/T06/T07 要求仍然没有逐一满足。

## 7.4 B5 批次（R13/R14/R15）的可复核入口

```sh
# 诊断日志：字段、上限、轮转、不泄密
go test ./internal/diag ./cmd/dustkeeper -run 'Log|Record' -count=1 -v

# 救援与"不可读≠不存在"
go test ./internal/rescue ./test/integration -run 'Inventory|Unreadable|MissingObjects' -count=1 -v

# 发布产物：真实 musl 证据、静态链接、冒烟
go test ./test/release -count=1 -v
./build/build.sh                     # dev：本机目标 + build-facts.txt
./build/build.sh --release           # 缺目标/缺证据 → 退出 1

# 性能证据（会真的建树，默认不跑）
DUSTKEEPER_BENCH=1 DUSTKEEPER_BENCH_FILES=100000 \
  DUSTKEEPER_BENCH_OUT=docs/evidence/performance-100k.md \
  go test ./test/bench -run TestLargeTreeScanEvidence -count=1 -v
```

`docs/evidence/*.md` 由上面的命令生成（文件头写明 "generated by test/bench; do not edit by hand"），
同目录的 `.json` 是同一份数据，便于脚本比对。

