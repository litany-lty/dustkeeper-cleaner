# Dustkeeper 长期无人值守运行体检与完整修改方案（2026-11-25）

> 基线提交：`85e99db`（schema 6，不主动破坏既有 schema 6 兼容性与既有安全边界）
> 目标：消除阻止 Dustkeeper **长期独立无人值守运行、安全持续清理系统垃圾**的代码缺陷、状态卡死与自膨胀隐患。
> 约束：不修改线上主机配置/状态、不改动已有未跟踪文档 `docs/tasks/dustkeeper-capability-roadmap-20261115.md`、不自动提交/推送；每项修改均遵循 TDD（先写最小复现用例/确认失败，再做最小根因修复并定向回归验证）。

---

## 一、修复进度总览（实时更新）

| 编号 | 批次 | 优先级 | 问题摘要 | 状态 | 主要修改文件 | 验证记录 |
|---|---|---|---|---|---|---|
| **R01** | 批次一 | P0 | `OpenDispatchFor` 按 `item_id` 查询导致定时重做计划绕过未结算防重付 | `done` | `internal/store/review_dispatch.go`, `internal/execute/keep.go`, `internal/store/review_dispatch_test.go`, `test/integration/remediation_batch1_test.go` | `TestPreparedRequest*`、`TestR01PendingDispatchBlocksReplannedScheduledItem` 通过 |
| **R02** | 批次一 | P0 | `purge` 到期清空未复用 `keptRecently`，且 `keep` 缺少按动作隔离与分页（500 条截断） | `done` | `internal/store/intents.go`, `internal/plan/keepmatch.go`, `internal/execute/keep.go`, `internal/execute/purge.go`, `internal/run/runner.go` | `TestR02PurgeReusesKeepAndDoesNotReaskDaily`、`TestR03*`（>500 噪声事件）通过 |
| **R03** | 批次一 | P0 | 路线图 W1 + 大文件读预算：`CreateRulePlan` 规划前过滤有效 keep、未决派发与 `2×` 超读预算项 | `done` | `internal/plan/planner.go`, `internal/plan/keepmatch.go`, `internal/policy/policy.go`, `internal/store/review_unknown.go`, `internal/run/runner.go` | `TestR01*`、`TestR03KeptItemDoesNotStarveNextCandidateAndSurvives500Events`、`TestReviewCycle*`（14 项）通过 |
| **R04** | 批次一 | P1 | 本地 `!Complete \|\| !Movable` 仍发付费模型请求；改为审前短路 + 单文件定向 `lsof`/`/proc` 探测 + `doctor` 自检 | `done` | `internal/execute/quarantine.go`, `internal/execute/facts.go`, `internal/assess/probe.go`, `internal/assess/probe_linux.go`, `internal/assess/probe_other.go`, `cmd/dustkeeper/commands.go` | `TestProbeOpenPathTargetsSingleFile`、`TestR04LocalFactsFailureShortCircuitsBeforePaidReview`、`TestUnknownProcessFact*` 通过 |
| **R05** | 批次一 | P1 | `Runner.purgeDue` 在提醒失败或建计划失败时未归还已预占的 grant 额度；修正提醒文案及非 darwin `doctor` 预警 | `done` | `internal/run/runner.go`, `cmd/dustkeeper/commands.go` | `TestR05ReminderFailureReleasesPurgeGrantQuota`、`TestDoctorAcknowledgingTheClockClearsTheLatch` 通过 |
| **R06** | 批次二 | P0 | 单个不可读子目录导致整个 scope 永久卡死在旧 epoch 且停止全部新隔离（保留 R11 不误退观察记录） | `done` | `internal/store/observations.go`, `internal/run/runner.go`, `test/integration/remediation_batch2_test.go` | `TestAFailedDirectory*`、`TestSlimIncomplete*`、`TestR06FailedSubdirDoesNotFreezeHealthySubdirOrNewFileDiscovery` 通过 |
| **R07** | 批次二 | P0 | macOS 跨重启 `st_dev` 漂移导致 `OpenVault` 报错中断；支持 macOS 真实持久卷 UUID（`ATTR_VOL_UUID`）与安全兼容 | `done` | `internal/domain/types.go`, `internal/policy/guard.go`, `internal/fsops/{fsops,platform_darwin,platform_linux,private}.go`, `internal/scope/{identity,vault,rebind}.go` | `TestR07DarwinVolumeUUIDIsPersistentAndConsistent`、`TestR07PersistentVolumeUUIDSurvivesRebootDeviceRenumbering`、`TestScopeRebind*`、`GOOS=linux go vet` 通过 |
| **R08** | 批次二 | P0 | 48 小时时钟跨度锁存不可配置，导致休眠/周末后永久停摆；增加可配置 `clock_max_gap_hours`（默认仍 48h 保兼容） | `done` | `internal/config/{config,config_test}.go`, `internal/clocktrust/{clock,clock_test}.go`, `internal/run/runner.go`, `cmd/dustkeeper/{commands,cmd_object,cmd_plan}.go` | `internal/config`、`TestR08ConfigurableMaxGapAllowsExpectedOfflineWindow`、`TestR08ClockMaxGapHoursConfigAllowsOfflineWindow`、`TestClockAck*` 通过 |
| **R09** | 批次二 | P1 | `diskPressure` 跨卷全局连坐且阻塞直删；改为按当前 scope 所在卷独立判定，压盘时允许直删仅暂停新隔离 | `done` | `internal/policy/policy.go`, `internal/plan/planner.go`, `internal/run/runner.go`, `test/integration/remediation_batch2_test.go` | `TestDiskPressurePausesQuarantineButNotADuePurge`、`TestR09DiskPressurePerScopeAndAllowsDirectDelete` 通过 |
| **R10** | 批次三 | P1 | 清理器自膨胀与查询退化：`<state>/reports` 轮转、`eventsInWindow` 下推 SQL、`stale` 观测定期清理、LaunchAgent `Umask` | `done` | `internal/report/{render,collect,rotation_test}.go`, `internal/store/{intents,observations}.go`, `internal/run/runner.go`, `internal/schedule/{render,render_test}.go` | `internal/report`（含 `TestR10*`）、`internal/schedule` 通过 |
| **R11** | 批次三 | P0/P1 | 调度包装器与部署加固：`run-readable-log.py` 优雅 `SIGTERM` 超时、`run-scheduled-log.py` 回执验真加固 + 零 HTTP 可重试不永久暂停 + 可配置账本约束、`deploy/elidli` 去除 root 与静默吞错 | `done` | `scripts/{run-readable-log,run-scheduled-log,test_run_scheduled_log,test_run_readable_log}.py`, `deploy/elidli/dustkeeper-elidli.service` | Python 单测 35/35 全绿（含既有 3 项可读日志单测 + `SIGTERM` 优雅退出、回执验真、零 HTTP 环境退出、账本自定义限额） |
| **R12** | 批次三 | P2 | `packaging/install.sh` 符号链接解析、八进制 `08`/`09` 修复、macOS 重复安装 `bootout`；`.github/workflows/ci.yml` 接入 Python 单测 | `done` | `packaging/install.sh`, `.github/workflows/ci.yml` | `sh -n packaging/install.sh` 与 `--dry-run --hour 08 --minute 09` 符号链接实测通过 |
| **R13** | 批次三 | P2 | 核对供应商响应严格白名单与 JEV 代理约束：保留既有安全契约不弱化，补充脱敏结构化错误分类（`UNKNOWN_FIELD` 等） | `done` | `internal/review/clients.go` | `internal/review` 全包回归通过（含 `TestJEVExplicitProxyDoesNotRouteMiniMaxOrFallBack`） |
| **R14** | 批次四 | P0 | 隔离区 `.dustkeeper/manifests/` 终态回收与 `.dustkeeper/tmp/` 孤儿临时文件清扫 | `done` | `internal/execute/{purge,restore,vault}.go`, `internal/recovery/reconcile.go`, `internal/run/runner.go` | `TestR14PurgeAndRestoreRemoveManifestsAndSweepOrphanTmp`、`internal/execute`、`internal/run` 及 `reconcile`/manifest 集成回归全绿 |
| **R15** | 批次四 | P0 | `<state>/run.log` 无界增长轮转（与 `dustkeeper.jsonl` 同等级别安全私有轮转） | `done` | `internal/diag/log.go`, `cmd/dustkeeper/commands.go` | `TestR15RotateFileIfOversize`（`internal/diag`）、`go build ./cmd/dustkeeper` 及 `cmd/dustkeeper` 定向回归全绿 |
| **R16** | 批次四 | P0 | 多 Scope 场景下模型端点不可用 / 鉴权失败的跨 Scope 整轮熔断 | `done` | `internal/run/runner.go` | `TestR16ModelOutageStopsAcrossMultipleScopes` 及 `TestModelOutageIsNotACleanSweep` 全绿 |
| **R17** | 批次四 | P0 | 无人值守停摆 24h 限频告警 + 原子 `<state>/health.json` 状态快照 + `doctor` 自检 | `done` | `internal/run/{health,runner}.go`, `cmd/dustkeeper/commands.go` | `TestR17StallAlertIsRateLimitedAndHealthFileUpdates`、`doctor`/通知回归及 `internal/run` 全绿 |
| **R18** | 批次四 | P1/P2 | 凭据分类阻断与黑名单扩展 + 可配置高收益候选排序（`candidate_order`: `path`/`size_desc`/`oldest_first`） | `done` | `internal/assess/classify.go`, `internal/config/config.go`, `internal/store/observations.go`, `internal/plan/planner.go`, `internal/execute/quarantine.go` | `TestR18CredentialShortCircuitAndCandidateOrderSizeDesc` 及 `assess`/`config`/`plan`/`store`/`execute` 测试全绿 |

---

## 二、逐项详细修改方案

### 批次一：防重付、规划阻塞与审前链路

#### R01：`OpenDispatchFor` 改为按 `(scope_id, action, identity_json, token_json)` 查重
- **根因**：`internal/store/review_dispatch.go` 中 `OpenDispatchFor` 使用 `WHERE d.item_id=?` 且先 `LIMIT 4` 再比对 identity/token，导致定时 `CreateRulePlan` 生成新 `item_id` 后完全绕过未结算派发阻塞。
- **方案**：
  1. 将 `OpenDispatchFor` 查询改为按 `d.scope_id=? AND d.action=? AND d.state='pending' AND d.identity_json=? AND d.token_json=? AND r.dispatch_id IS NULL` 精确匹配（移除截断性的 `LIMIT 4`，改为直接 SQL 匹配或合理上限）。
  2. 更新 `internal/execute/keep.go` 传入 `string(sc.Scope.ID)`，同步更新 `internal/store/review_dispatch_test.go` 并新增跨 `item_id` 重做计划仍被阻塞的回归用例。
- **兼容性**：复用 schema 005 既有列，零迁移。

#### R02：`purge` 复用 `keptRecently` + `keep` 按动作隔离 + 消除 500 条截断
- **根因**：
  1. `KeepEventsSince(ctx, sinceNS, 500)` 全局只读最近 500 条 keep，高频运行后早先有效 keep 会被挤出窗口。
  2. `keep` 事件的 `ContractJSON` 未记录 `action`，且 `Executor.Purge` / `Runner.purgeDue` 从不查 `keptRecently`，导致到期清空被模型拒删后每天重复付费并阻塞队列。
- **方案**：
  1. `Store` 新增分页/作用域查询方法 `KeepEventsForScopeSince(ctx, scopeID, sinceNS)`（或按 `sequence` 倒序分页扫描），消除 500 条硬截断。
  2. `recordKeep` 在 `ContractJSON` 写入 `"action"`（旧事件无 `"action"` 字段时默认视为非 `purge` 隔离/直删判定，保持向后兼容）。
  3. 抽出纯匹配函数（供规划期与执行期复用一份事实来源），校验 `scope_id`、`raw_relative`、`action`、`rule_digest`（隔离链路）、`identity`、`token`、非失败结论、未过期。
  4. `Runner.purgeDue` 在收集 `authorised` 前跳过命中有效 `purge` keep 或未决 `purge` 请求的到期项（写入 `ExpirySkipped` 说明原因，不占额度）；`Executor.Purge` 执行前同样做 `keptRecently` 复核。

#### R03：路线图 W1（规划前过滤）+ 大文件 `2×` 读预算前置过滤
- **根因**：
  1. `CreateRulePlan` 按路径字典序取前 `MaxFiles` 个匹配项，把有效 keep / 未决派发塞进唯一名额，后续合格文件永远排不到。
  2. `MaxAutoQuarantineBytes=1GiB` 与 `RunReadBudgetBytes=1GiB` 但执行器需 `2×` 读校验，导致 >512 MiB 文件入选后在审前直接触发 `BUDGET_EXHAUSTED` 卡死 scope。
- **方案**：
  1. `policy` 新增原因码 `KEPT_RECENTLY`、`AWAITING_REQUEST_CHECK`、`READ_BUDGET_TOO_SMALL`。
  2. `RulePlanRequest` 增加可选字段 `SkipKept`、`SkipPending`、`MaxReadBudgetBytes`、`ExcludedDirPrefixes`（`Runner` 自动计划开启，人工预览/显式计划默认保持原语义兼容）。
  3. 在进入 `IterateObservations` 单连接迭代器**之前**，批量预取本 scope 的有效 keep 摘要与未决 `review_unknown`/`review_dispatch` 摘要，在迭代回调内纯内存过滤后再决定是否占 `MaxFiles`/`MaxBytes`。
  4. 当所有匹配项均因上述原因被跳过而无候选时，`ScopeRun.Message` / `QuarantineSkipped` 输出结构化分布计数（例如多少在保留期、多少等人工核查、多少超出读预算），避免无解释空结果。

#### R04：本地事实不足审前短路 + 轻量单文件打开探测 + `doctor` 自检
- **根因**：
  1. `quarantine.go` 在 `measureFacts` 返回 `!Complete || !Movable` 后仍调用付费 JEV + MiniMax，结果又在 `outcome.Decide` 必被判 `KEEP`。
  2. `ProbeOpenUnder` 对整棵目录运行 `lsof +D <root>`（10s / 256 KiB），大目录易超时或截断，精简 Linux 缺 `lsof` 时直接静默不可移动。
- **方案**：
  1. 在 `quarantine.go` 中调用 `prepareReview` 之前，若 `!facts.Complete || !facts.Movable`，直接短路写 `diagNoRequest` + `recordKeep`（原因分别为 `facts_missing` / `move_safety_unknown`，`CostNotSent`，0 次 HTTP），不调用模型。
  2. 新增 `assess.ProbeOpenPath(ctx, path, run)`：
     - Linux 优先遍历 `/proc/<pid>/fd` 按 `(dev, ino)` 精确核查目标文件是否被可见进程打开（零外部命令依赖、无 `lsof +D` 超时问题）；无法访问 `/proc` 时回退 `lsof -- <path>`。
     - macOS 对单候选文件调用 `lsof -nP -w -F0n -- <path>`（毫秒级、不遍历整棵树），避免 `+D` 10s 超时与 256 KiB 截断；保留 `e.OpenFiles` 注入接口供现有测试使用。
  3. `cmdDoctor` 增加 `process_probe` 检查项，启动阶段即提示进程打开文件探测是否可用。

#### R05：`purgeDue` 额度泄漏修复 + 通知门禁提示优化
- **根因**：`runner.go:588-596` 当 `remindBeforeClearing` 返回 `!ok` 或 `CreatePurgePlan` 报错时，已 `gl.Add(size)` 的 grant 额度未释放；Linux 未设 `--notify none` 时默认调 `osascript` 会延迟到 7 天后才失败。
- **方案**：
  1. 抽取 `releaseAllClaimed()`，在 `remindBeforeClearing` 失败与 `CreatePurgePlan` 失败路径统一释放预占额度。
  2. 优化 `remindBeforeClearing` 提醒文案，准确反映“本次即将清空（如需停止可立即 `pause` 或查看结果页）”，不误导称“删除前可恢复”。
  3. `cmdDoctor` 在非 darwin 平台且策略为 `macos_notification` 时发出明确诊断提示（给出 `cleanup setup --notify none` 建议）。

---

### 批次二：扫描韧性、重启卷身份、时钟与磁盘门禁

#### R06：单个不可读子目录不永久冻结整棵 scope（严格保留 R11）
- **根因**：`failed` 目录计入 `Pending`，`CurrentEpoch` 永远复用同一 `scan_epoch`，已 `done` 的根目录与其他目录永不再扫，且 `runScope` 见到 `!scanState.Complete` 直接跳过全部隔离。
- **方案**：
  1. 保持 R11 全部不变量：`failed` 目录留在原 `scan_epoch`、`state.Complete=false`、`state.Incomplete=true`、`ScanEpoch` 不递增、不把失败目录下的旧观测退为 `stale`。
  2. 新增 `Store.ResetDoneDirsForRefresh(ctx, scopeID, epoch)`：当新一轮进入 `CurrentEpoch` 且发现当前 epoch 已无 `pending`/`active` 目录、**仅剩 `failed` 目录**时，将 `done` 目录重新置为 `pending`（保持 `failed` 目录仍为 `failed`），使本轮既重试失败目录，又能发现其他可读目录中新增的垃圾文件；且不触发 `MarkStaleObservations`。
  3. `runScope` 当 `!scanState.Complete` 时区分两种情况：
     - 若 `scanState.Interrupted` 或仍有非失败的未扫目录（`Pending > FailedDirs`）：继续跳过隔离（全树尚未走完）。
     - 若仅存在失败目录（`!Interrupted && FailedDirs > 0 && Pending == FailedDirs`）：读取 `FailedDirs` 列表传给 `CreateRulePlan` 的 `ExcludedDirPrefixes`（排除所有失败子树），允许对其余可读目录中已观测候选正常执行隔离，并在 `sr.Message` 明确报告失败目录数。

#### R07：macOS 真实持久卷 UUID（`ATTR_VOL_UUID`）+ `OpenVault` 跨重启兼容
- **根因**：macOS/Linux 使用 `dev-<st_dev>` 作为 `VolumeID`，重启重排设备号后 `vault.go:297` 用新 `identity` 硬比对数据库旧 `expect.Identity` 导致每次重排都中断自动任务。
- **方案**：
  1. `internal/fsops/platform_darwin.go` 新增 `volumeUUIDOfFD(fd int) (string, bool)`，通过 macOS 原生 `SYS_FGETATTRLIST` 读取 `ATTR_VOL_UUID`（16 字节格式化为标准 UUID 字符串），供 `Root.Identity()` 写入持久 `VolumeID`（若文件系统不支持则回退 `dev-<st_dev>`）。
  2. 审慎核查 `internal/scope/rebind.go` 与 `internal/scope/vault_test.go`：`OpenVault` 在 `legacyRelabel` 已通过旧对象摘要与旧 anchor 摘要双校验时，保留 `scope rebind` 的显式重绑定安全契约，同时让新注册或执行过一次 `scope rebind` 的 macOS scope 自动升级为真实持久 `ATTR_VOL_UUID`，从此彻底免疫跨重启 `st_dev` 漂移！

#### R08：可配置时钟最大间隔 `clock_max_gap_hours`（默认仍 48h）
- **根因**：`JumpThreshold = 48 * time.Hour` 硬编码，笔记本休眠周末/出差超过两天即永久锁存并阻断自动清理。
- **方案**：
  1. `config.Runtime` 新增可选字段 `ClockMaxGapHours int`（JSON `clock_max_gap_hours,omitempty`；省略或 `0` 表示默认 48 小时，有效范围 `1..720` 小时即最长 30 天，仍会拦截 60 天异常跨度与所有时钟倒退/运行内漂移）。
  2. `clocktrust.Guard` 增加 `MaxGap time.Duration`（零值回退 `JumpThreshold = 48h`），`Runner` 与 `cmdDoctor` 统一读取 `cfg.Runtime.ClockMaxGap()`，保持单一事实来源并完全兼容全部既有 48h 测试。

#### R09：`diskPressure` 按 scope 所在卷独立判定，压盘时允许直删
- **根因**：`diskPressure()` 遍历全局所有 mutable scope，单卷满或外置盘离线连坐其他卷；且压盘时连能立即释出空间的 `deleteDirect` 也一并跳过。
- **方案**：
  1. 新增 `Runner.diskPressureFor(sc store.Scope)`，仅检查当前 scope 所在卷（`Report.DiskPressure` 仍按全局任一受压汇总用于报告）。
  2. 当当前卷处于磁盘压力时：
     - 若该 scope 不具备已确认的直删类别或无有效 `auto_purge` 授权，继续跳过隔离（完全保留 `TestDiskPressurePausesQuarantineButNotADuePurge`）。
     - 若存在已批准直删类别与有效 `auto_purge` 授权，仅允许 `deleteDirect` 分支执行、在 `quarantineOnly` 处保持由 `checkQuarantineSpace` 阻止新隔离，从而在磁盘紧张时真正能释放空间。

---

### 批次三：消除自膨胀、全表扫描与加固运维调度链

#### R10：消除报告/日志/观测无界增长与 `events` 全表扫描
- **方案**（全程保持 schema 6 兼容，不强制升级线上库）：
  1. `internal/report/render.go`：`report.Write` 写入 `result-<run_id>.md/.json` 后自动轮转清理同目录下本工具命名的历史快照（保留最近 `MaxReportSnapshots = 30` 轮且总字节不超过 `20 MiB`，绝不触碰 `latest.*` 与其他文件）。
  2. `internal/store/intents.go` + `internal/report/collect.go`：新增 `Store.EventsInWindow(ctx, startNS, endNS)`，将时间窗口过滤下推到 SQLite 引擎（避免每轮从 `sequence=0` 反序列化全部历史事件到 Go 内存）。
  3. `internal/store/observations.go` + `internal/run/runner.go`：新增 `Store.PruneStaleObservations(ctx, scopeID, olderThanNS)`，在完整 epoch 收尾后清理 `status='stale'` 且 `last_seen_ns` 超过 30 天的死观测行。
  4. `internal/schedule/render.go`：macOS LaunchAgent plist 增加 `<key>Umask</key><integer>63</integer>`（即 `0077` 私有权限），并在 README 提示定期清理或改由受控日志包装器承载 stdout。

#### R11：包装器与 `deploy/elidli` 长期无人值守加固
- **方案**：
  1. `scripts/run-readable-log.py`：用 `subprocess.Popen` 替换 `subprocess.run(..., timeout=timeout)`；超时先发 `SIGTERM` 等待优雅收尾（最多 30 秒，确保 `dustkeeper` 完成当前持久化落盘与 `review_dispatch` 结算），仅在仍未退出时 `SIGKILL`。
  2. `scripts/run-scheduled-log.py`：
     - 补齐 `_receipt` 三项验真缺陷：① 拒绝非字符串/`None` 的 `run_id`；② 校验 `ended_at <= now + 60s` 上界；③ `exit == 9` 要求 `scopes` 为非空列表。
     - 区分“零 HTTP 且无未知费用的环境暂阻”（如 `INSTANCE_BUSY=3`、`CLOCK_UNTRUSTED=6`、`DISK_PRESSURE=7`、以及 `http_attempts==0` 且无未知结果的 `PARTIAL`）：退还预留额度、清 `pending`、返回非零退出码供 systemd/日志告警，**不永久暂停实例**；任何 `http_attempts > 0` 失败、未知费用、坏回执或 `IO_ERROR` 仍保持扣留预留并暂停。
     - 支持账本可选 `limits` 与 `minimax_key_prefix` 配置（缺省沿用现有默认 `1/1/2` 与 `sk-cp-`），不再把扩容与换 key 写死在脚本里。
  3. `deploy/elidli/dustkeeper-elidli.service`：
     - 将 `User=root` 改为专用低权限服务账号模板 `User=dustkeeper`（并加注说明现有 root 实例迁移权限命令）。
     - 移除会静默吞掉暂停状态的 `ConditionPathExists`（由 `run-scheduled-log.py` 明确输出 `REFUSED` 并返回非零码，便于 systemd 告警/排查），配置 `KillSignal=SIGTERM`、`TimeoutStopSec=60`、`TimeoutStartSec=1200`（严宽于包装器 960s）。

#### R12：`packaging/install.sh` 与 `.github/workflows/ci.yml` 修复
- **方案**：
  1. `packaging/install.sh`：用 POSIX 符号链接循环 + `cd -P` 真正解析二进制真实路径；去除 `$HOUR`/`$MINUTE` 前导零避免 `/bin/sh` 把 `08`/`09` 当八进制报错；macOS 安装前对已加载任务安全执行 `launchctl bootout ... 2>/dev/null || true`。
  2. `.github/workflows/ci.yml`：在双平台 CI 中新增 `python3 -m unittest discover -s scripts -p 'test_*.py'` 步骤，守住生产包装器回归防线。

#### R13：供应商响应严格白名单与 JEV 代理核对
- **核对结论与实施方案**：
  1. `internal/review/diagnostics_test.go` 中 `TestJEVExplicitProxyDoesNotRouteMiniMaxOrFallBack` 明确锁定安全契约：配置显式 JEV 代理后，代理不可达时绝不静默回退直连或串扰 MiniMax 链路；予以原样保留。
  2. `internal/review/clients.go` 的 `strictjson.Allow` 白名单校验与零原文泄露脱敏策略属于核心安全边界，予以保留；同时在 JEV `usage`、MiniMax 决策 JSON 与 MiniMax `usage` 拒绝处追加安全的 `strictjson.Error.Kind` 分类标识（如 `UNKNOWN_FIELD` / `MISSING_FIELD`），既不泄露任何原始响应内容或未知字段值，又能在供应商变更字段结构时快速定位原因。

---

### 批次四（第二轮深度加固）：隔离区回收、日志轮转、跨 Scope 熔断、健康告警与高收益调度（R14–R18）

#### R14：隔离区 `manifests/*.json` 终态清理与 `.dustkeeper/tmp/*` 孤儿回收
- **根因**：`Executor.Purge` / `Executor.Restore` 及 `recovery.Reconciler` 完成对象终态（`purged`/`restored`）或确认 `q_intent` 未移动而中止后，未清理 `.dustkeeper/manifests/<object>.json`，导致清单目录长期无界增长并使 `rescue.Inspect` 误报 `manifest_without_object`；此外崩溃遗留在 `.dustkeeper/tmp/` 的临时文件缺少回收。
- **方案**：
  1. 严格守住不变量：仅在 `CompletePurge` 或 `CompleteRestoreReleasingHold` **成功落库提交之后**，或在 `reconcile` 确认 `q_intent` 从未进入 `objects` 并中止为 `aborted` 后，尽力移除对应 manifest（`quarantined`/`held`/`r_intent`/`p_intent`/`unknown` 对象清单绝不删除）。
  2. `ScopeCtx` 新增 `SweepOrphanTmp(now, maxAge)`，仅在 `ScopePendingOps == 0` 时安全清理超过 24 小时且为单链普通文件的 `.dustkeeper/tmp/*` 孤儿（`AT_SYMLINK_NOFOLLOW` + `NLink == 1`）。

#### R15：macOS LaunchAgent `<state>/run.log` 有界轮转
- **根因**：`dustkeeper.jsonl` 已按 `2 MiB × 3` 轮转，但 launchd 输出重定向的 `<state>/run.log` 长期追加无上限。
- **方案**：在 `internal/diag` 新增 `RotateFileIfOversize(dir, name, maxBytes, keep)`，沿用私有安全目录打开与 `AT_SYMLINK_NOFOLLOW`/属主/权限核验，在 `cmdRun` 结束时按需轮转 `<state>/run.log`（`2 MiB × 3`）。

#### R16：多 Scope 模型端点故障 / 鉴权失效整轮熔断
- **根因**：模型返回 `CodeModelUnavailable`（如 401/403、代理离线、超时未知）时仅返回 `ControlStopScope`，导致多 scope 实例在后续 scope 上重复发起必败请求并制造更多 `review_unknown` 冻结记录。
- **方案**：保持 `TestModelOutageIsNotACleanSweep` 对单 scope `ControlStopScope` 的测试契约不变；在 `Runner.Run` 遍历 `ordered` scopes 时，一旦某 scope 因 `CodeModelUnavailable` 终止，将剩余未执行 active scopes 计入 `UntouchedScopes` 并立即 `break` 整轮循环。

#### R17：无人值守停摆 24h 限频告警 + 原子 `<state>/health.json` 状态快照 + `doctor` 检查
- **根因**：停摆异常（时钟失信、scope 被 `held`、未结算挂起、状态落库失败）无主动通知会造成多日静默停摆，且状态目录缺少供外部巡检的稳定健康摘要文件。
- **方案**：
  1. 在 `internal/run/health.go` 定义 `HealthSnapshot` 并原子写入 `<state>/health.json`（含 `healthy`、`consecutive_unhealthy_runs`、`last_healthy_run_ns`、`reasons`）；健康 0 动作轮次绝不发通知。
  2. `Runner.Run` 收尾调用 `maybeNotifyStall`，仅在存在可行动停摆签名且本轮通知通道未失败时，按 `meta["stall_alert.v1"]` 做同签名 24 小时限频告警；恢复健康后自动清零去重状态；`cmdDoctor` 补充 `last_run_health` 提醒。

#### R18：凭据分类强保护 + 可配置高收益候选排序（`candidate_order`）
- **根因**：高危凭据扩展名不够完整且规划/执行期未直接按 `CatCredential` 拦截送审；`IterateObservations` 仅支持路径字典序，低限额下大文件容易排在小文件之后。
- **方案**：
  1. 扩充 `isCredentialName`（`.netrc`, `.pgpass`, `.my.cnf`, `credentials*`, `application_default_credentials.json`, `kubeconfig`, `terraform.tfstate*`, `.keystore`, `.jks`, `.ovpn`, `.asc`, `.age`），在 `Planner.CreateRulePlan` 与 `Executor.process` 零模型调用直接拦截 `CatCredential`（保持 `policy.PolicyVersion=4` 不失效已有计划）。
  2. `config.Runtime` 新增可选字段 `candidate_order`（`""`/`"path"` | `"size_desc"` | `"oldest_first"`），在 `store.IterateObservationsOrdered` 中分别使用既有主键及 `observations_by_size` / `observations_by_age` 索引，兼顾全量向后兼容与大体积优先清理。

---

## 三、变更与验证日志

> 以下按实施顺序逐项追加，只记录真实改动与实测命令结果。

- **2026-11-25 16:30**：完成方案建档，开始执行批次一（R01–R05）。
- **2026-11-25 16:41**：批次一（R01–R05）全部完成并通过定向验证（`internal/store`、`internal/execute`、`internal/assess`、`cmd/dustkeeper` 及 `test/integration` 新增 `TestR01`–`TestR05` 与 18 项关联回归用例全绿）。开始执行批次二（R06–R09）。
- **2026-11-25 16:56**：批次二（R06–R09）全部完成并通过定向验证（`internal/fsops`、`internal/scope`、`internal/clocktrust`、`internal/config`、`cmd/dustkeeper`、`test/integration` 新增 `TestR06`–`TestR09` 及既有 `scan_epoch`/`slim_f4`/`gate`/`scope_rebind` 回归全绿，且 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/fsops ./internal/assess` 通过）。开始执行批次三（R10–R13）。
- **2026-11-25 17:03**：批次三（R10–R13）全部完成并通过定向验证（`internal/report`、`internal/schedule`、`internal/review`、Python 脚本单测、`sh -n packaging/install.sh` 与 `--dry-run --hour 08 --minute 09` 符号链接实测全绿）。进入最终统一回归与文档终态核对。
- **2026-11-25 17:06**：最终定向回归全部通过：
  - `go vet ./...`、`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/fsops ./internal/assess`、`go build -o /dev/null ./cmd/dustkeeper` 通过；
  - `internal/domain`、`internal/policy`、`internal/plan`、`internal/run`、`internal/fsops`、`internal/scope`、`internal/clocktrust`、`internal/config`、`internal/assess`、`internal/report`、`internal/schedule`、`internal/review` 全包单测通过；
  - `internal/store`、`internal/execute`、`cmd/dustkeeper` 及 `test/integration` 定向回归（含 `TestR01`–`TestR09`、`TestR02` 连续 `-count=5` 消除同秒时间戳并列抖动及既有 `scan_epoch`/`slim_f4`/`gate`/`scope_rebind`/`review_cycle` 回归）全部通过；
  - `python3 -m unittest discover -s scripts -p 'test_*.py'` 35/35 全绿；`sh -n packaging/install.sh` 与 `--dry-run --hour 08 --minute 09` 符号链接实测通过。
  - **未执行项**（按增费/发布授权边界留待确认）：全量 `go test ./...`、`go test -race ./...`、正式发布构建（`./build/build.sh --release` / `./build/build-linux-release.sh`）、以及线上本机/`elidli` 实机部署与旧 macOS `dev-*` scope 的一次性 `scope rebind` 升级。
- **2026-11-25 17:19**：开启第二轮深度加固（批次四：R14–R18）。
- **2026-11-25 17:29**：批次四（R14–R18）全部实现并通过统一验证回归：
  - **已实现**：
    - `R14`：终态提交（purge / restore / reconcile 终态）回收 `.dustkeeper/manifests/<object>.json`，并在无待定操作（`ScopePendingOps == 0`）时清理超过 24h 的单链接 `.dustkeeper/tmp/*` 孤儿文件；
    - `R15`：新增 `diag.RotateFileIfOversize` 并在 `cmdRun` 入口对 `<state>/run.log` 执行 `2 MiB × 3` 安全私有轮转；
    - `R16`：`Runner.Run` 在首个活跃 Scope 触发 `errs.CodeModelUnavailable` 时立即熔断同轮剩余活跃 Scope；
    - `R17`：新增 `internal/run/health.go`，提供基于 `meta["stall_alert.v1"]` 的 24h 限频无人值守停摆告警、原子写入 `<state>/health.json`（保留既有字段与 `last_run_health` 状态），并在 `cmdDoctor` 增加建议性健康检查；
    - `R18`：扩充 `isCredentialName` 黑名单并在 `Planner.CreateRulePlan` 与 `Executor.process` 零模型调用直接拦截 `CatCredential`，同时为 `Runtime` 新增 `candidate_order`（`""`/`"path"` | `"size_desc"` | `"oldest_first"`）并由 `store.IterateObservationsOrdered` 落地排序查询。
  - **已验证**（全部退出码 0）：
    - `go vet ./...`、`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/fsops ./internal/assess`、`go build -o /dev/null ./cmd/dustkeeper` 通过；
    - `go test` 受影响核心包（`internal/diag`、`internal/assess`、`internal/config`、`internal/policy`、`internal/plan`、`internal/run`、`internal/rescue`、`internal/report`、`internal/schedule`、`internal/review`、`internal/fsops`、`internal/scope`、`internal/clocktrust`、`internal/store`、`internal/execute`；`internal/recovery` 无独立单测文件）全绿；
    - `cmd/dustkeeper` 定向回归（`TestScopeRebind|TestClockAck|TestDoctor|TestRunClock|TestRecover|TestSlim|TestAConfirmedChoice|TestTheSameSeam`）与 `test/integration` 定向回归（含 `TestR01`–`TestR09`、`TestR14`、`TestR16`–`TestR18` 及既有 `reconcile`/通知/模型故障/磁盘压力/预算/评审循环回归）全部通过；
    - `python3 -m unittest discover -s scripts -p 'test_*.py'` 35/35 全绿。
  - **未验证 / 需单独授权项**：全量 `go test ./...`、竞态检测 `go test -race ./...`、正式发布构建（`./build/build.sh --release` / `./build/build-linux-release.sh`）以及线上本机/`elidli` 实机部署。

---

## 四、本地 10 轮观察行为实测与优化记录（O01–O10）

> 范围与隔离约束：全部本地实测均在独立沙箱目录与本地回环自签 HTTPS 假模型端点运行，**真实外部模型调用为 0**，未读取或修改 `~/.local/state/dustkeeper`、`~/.config/dustkeeper`、`~/Downloads` 或任意生产数据；所有 `chmod 000` 与 `chflags uchg` 样本已在测试结束时自动还原。

| 轮次 | 本地观察发现的问题与核心改进 | 改动文件 | 本地实测与测试证据 |
|---|---|---|---|
| **O01** | 符号链接父路径与 macOS `lsof` exit-1 导致 `--open-files` 漏判活动句柄，且 `cleanup-report`/`debugrun` 使用易受 256 KiB 输出上限截断的全局列表；改用 `ProbeOpenUnder`、根路径双向规范化匹配与 `assess.Scan` 符号链接/inode 别名映射 | `internal/assess/{probe,scan}.go`, `cmd/cleanup-report/main.go`, `internal/debugrun/run.go` | `O01` 沙箱 `open_by_visible_process`: `false` → `true` (`keep`)；`TestO01OpenFileObservationResolvesAliasesAndExitOne` 与 `internal/assess`/`cmd/cleanup-report`/`internal/debugrun` 全绿 |
| **O02** | `assess.Scan` 进入 `.hg`/`.svn`/`.xcodeproj`/`.photoslibrary` 等受保护束目录并耗尽 `MaxEntries`，导致只读盘点与 `debug` 中断失败；补齐保护束/媒体库/VCS 跳过逻辑 | `internal/assess/scan.go` | `O02` 沙箱 `--max-entries 10`: `exit=9 (complete=false, skipped=0)` → `exit=0 (complete=true, skipped=3, 识别 z-old.tmp)`；`TestO02ProtectedBundlesAndVCSDoNotExhaustScanBudget` 通过 |
| **O03** | macOS `UF_IMMUTABLE`/`SF_IMMUTABLE`/`UF_APPEND`/`SF_APPEND`/`SF_DATALESS` 文件在只读盘点与受管扫描中未识别；补充 Darwin BSD flags 保护判定并在受管 `Scanner.Run` 提前跳过不可变或云端占位文件 | `internal/assess/{assess,scan}.go`, `internal/assess/flags_{darwin,other}.go`, `internal/fsops/{fsops,platform_darwin,platform_linux}.go`, `internal/scan/scanner.go` | `chflags uchg` 沙箱：`needs_review` + 入库 `1` → `immutable_or_append_only (keep)` + `scan skipped=1, obs=0`；`TestO03ImmutableAndCloudDatalessFilesStayKept` 与 `TestO03ScannerSkipsImmutableEntries` 通过 |
| **O04** | `quarantine`/`purge`/`delete` 完成事件缺失 `reason`/`review_id`，且 `report.Collect` 未关联 `review` 结算表导致已删/待删除及 0 次请求条目缺失 `http_attempts`/`cost_status`；补齐终态事件字段并将 `Entry.Attempts` 改为 `*int` 与结算事件关联 | `internal/execute/{quarantine,purge,delete}.go`, `internal/report/collect.go` | 混合批次沙箱：`deleted`/`observed` 找回 `reason`、`review_id`、`http_attempts: 2`、`cost_status`；`TestO04CollectPreservesReasonsReviewIDsAndZeroAttempts` 及 `internal/report`/`execute` 全绿 |
| **O05** | `MaxBytes` 截断在规划直方图中误记为 `CANDIDATE`，且零动作（含超限/过新/保护/保持期）未在 `dustkeeper run` 与 `ScopeRun.QuarantineSkipped` 解释；新增 `ReasonOverRunByteLimit` 与 `ScopeRun.CandidateCounts` 结构化漏斗输出 | `internal/policy/policy.go`, `internal/plan/planner.go`, `internal/run/runner.go` | `O05` 沙箱：`quarantine_skipped: null` → `"no eligible candidates acted on (1 exceed per-run byte limit, 1 modified too recently)"` + `candidate_counts`；`TestO05ZeroActionFunnelReportsOverByteLimitAndSkipBreakdown` 通过 |
| **O06** | `scripts/run-readable-log.py` 回执虽读入范围计数但在 Markdown 报告中省略分 scope 扫描/跳过/到期保留说明；扩展 `scope_rows` 字段（保持 `schema_version=3`）并渲染 `## 范围观察明细` | `scripts/run-readable-log.py` | 沙箱报告 `latest-readable.md` 展示各 scope 扫描完备性、动作及阶段跳过原因；`test_o06_scope_observation_details_rendered_in_markdown` 与 `scripts` 36 项单测全绿 |
| **O07** | `dustkeeper scan` 在部分完成/存在不可读子目录时仍输出矛盾的 `"status":"ok"` 与 `"code":"PARTIAL"`，且不披露失败目录路径；同步输出 `status="partial"` 与 `failed_dir_paths` | `cmd/dustkeeper/commands.go` | 沙箱 `chmod 000 unreadable_box`：`status: "ok", has failed_dir_paths: false` → `status: "partial", failed_dir_paths: ["unreadable_box"]`；`TestO07ScanPartialSetsStatusAndListsFailedDirs` 通过 |
| **O08** | `rule preview` 与 `plan create` 在 `NO_CANDIDATES` 时丢弃 `scanned` 与 `reasons` 分布；抽取公共输出辅助并输出各原因计数（`scanned=0` 提示先执行 `scan`），不改变任何摘要签名 | `cmd/dustkeeper/{cmd_rule,cmd_plan}.go` | 沙箱实测：`data: null` → `data: {"reasons":{"MODIFIED_TOO_RECENTLY":1},"scanned":1,"truncated":false}` 与可读行提示；`TestO08RulePreviewAndPlanCreateExposeReasonsOnNoCandidates` 通过 |
| **O09** | `dustkeeper status` 人类可读输出缺失 `observed=N` 与 `scan_interrupts=N`，且忽略 `health.json` 与未决审核 (`review_unknown`)；补充 `Store.ScanInterruptsOf` 与状态汇总展示 | `internal/store/observations.go`, `cmd/dustkeeper/commands.go` | 沙箱实测：输出 `observed=1 ... scan_interrupts=4`，JSON 增补 `unresolved_reviews` 与 `last_run_health`；`TestO09StatusReportsObservationsScanInterruptsAndHealth` 通过 |
| **O10** | `dustkeeper doctor` 未做只读双模型配置/凭据权限/非符号链接 CA 预检；新增零网络 `review_config` 检查（未配置时仍返回 `ok`，配置错误时返回 `MODEL_UNAVAILABLE`） | `cmd/dustkeeper/commands.go` | 沙箱 `0644` 凭据+符号链接 CA：`exit 0` → `exit 8 (MODEL_UNAVAILABLE)`，修正后 `exit 0 (validated)`；`TestO10DoctorValidatesReviewConfigReadOnlyWithoutNetwork` 通过 |

### 全链路本地沙箱预演与定向回归（2026-11-25 18:09）

- **全链路沙箱预演**（回环 HTTPS 假模型 + `scripts/run-scheduled-log.py` + `scripts/run-readable-log.py`）：
  - `doctor` 校验 `review_config` 通过；`cleanup-report --open-files --max-entries 10` 跳过 `Vacation.photoslibrary`，保留 `held_open.zip`（`open_by_visible_process`）与 `locked.tmp`（`immutable_or_append_only`）。
  - 第 1 轮 `run-scheduled-log.py`（`exit 0`, `http_attempts=5`, `reserved_millicny=10`）：混合执行删除 `removable.tmp`（`dual_review_agrees`, 2 attempts, `usage_recorded`）、隔离 `observable.zip`（`safe_to_observe`, 2 attempts, `usage_recorded`）与保留 `declined.zip`（`model_says_keep`, 1 attempt, `usage_recorded`）。
  - 第 2 轮 `run-scheduled-log.py`（`exit 0`, `http_attempts=0`, `reserved_millicny=0`）：跳过已保护/未满期对象并退回预留额度，生成包含 `## 范围观察明细` 的 `latest-readable.md`、`health.json`（`healthy: true`）与 `status`（`observation_count=3, scan_interrupts=0, quarantined_count=1`）。
- **已验证检查**：
  - `go vet ./...`、`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/fsops ./internal/assess`、`go build -o /dev/null ./cmd/dustkeeper`、`go build -o /dev/null ./cmd/cleanup-report` 全通过；
  - `go test` 目标包（`cmd/cleanup-report`、`internal/debugrun`、`internal/plan`、`internal/report`、`internal/run`、`internal/execute` 及前序实时通过/当前 `(cached)` 的 `internal/assess`、`internal/policy`、`internal/fsops`；`internal/scan` 无独立单测文件）全通过；
  - `cmd/dustkeeper` 定向回归（`TestO0[7-9]|TestO10|TestDoctor|TestStatus|TestScopeRebind|TestClockAck|TestRun|TestAConfirmedChoice|TestTheSameSeam`）通过（5.908s）；
  - `test/integration` 定向回归（`TestO0[3-5]|TestR0[1-9]|TestR1[4-8]|TestThreeOutcomes|TestQuarantineIsAnnounced|TestNothingTouched|TestModelOutage|TestAFailedDirectory|TestSlim`）通过（25.644s）；
  - `python3 -m unittest discover -s scripts -p 'test_*.py'` 36/36 全绿（2.625s）；`git status --short` 确认未改 `docs/tasks/dustkeeper-capability-roadmap-20261115.md`。
- **未验证 / 待授权项**：见下文第五节（已完成授权内的全量 `go test ./...`、`go test -race ./...`、`darwin-arm64` `--release` 构建及隔离沙箱真实双模型验证；仍待单独授权的仅剩本机真实实例升级与 `elidli`/Linux 容器发布及部署）。

---

## 五、真实费用门禁验证与真实双模型沙箱观察记录（2026-11-25 晚）

> 范围与隔离约束：用户授权「允许产生实际费用」后执行以下重型验证与真实双模型测试。所有真实模型调用均严格限定在独立隔离沙箱目录（`scratch/real-obs/`）与人工合成测试文件，**未读取、修改或删除任何真实用户文件，未改动 `~/.dustkeeper-review/*`、`~/Library/Application Support/Dustkeeper-Managed/*`、`~/.local/state/dustkeeper`、`~/Downloads` 或远端主机**；仅通过只读路径引用既有 `~/.dustkeeper-review/credentials.json`（从未打印或复制密钥内容）。

### 5.1 全量/竞态回归与期间发现并修复的 5 个问题

1. **`execute.TestHookDiskStatus` 补齐体积压力测试可重复性**（[hooks.go](file:///Users/litany/WorkBuddy/清理器/internal/execute/hooks.go#L102-L104)、[qcheck.go](file:///Users/litany/WorkBuddy/清理器/internal/execute/qcheck.go#L42)、[remediation_batch2_test.go](file:///Users/litany/WorkBuddy/清理器/test/integration/remediation_batch2_test.go#L186-L197)）：
   - **现象**：宿主 APFS 数据卷 `/System/Volumes/Data` 当前已用 ~92%（仅余 ~8% 空间），导致全量测试中 `TestR09DiskPressurePerScopeAndAllowsDirectDelete`（`min_free_percent: 10`）在 `execute.checkQuarantineSpace` 直接 `statfsFD` 时被真实宿主盘拒绝。
   - **修复**：新增 `execute.scopeStatFS` / `execute.TestHookDiskStatus`（生产默认 `nil`，保持直接对 `.dustkeeper/objects` 打开描述符调用 `unix.Fstatfs`），在多卷压力测试中同步注入 `run` 与 `execute` 两端钩子。
2. **规划期 `SkipPending` 跳过的未决审核计入 `UnknownHeld` 与日志门禁**（[runner.go](file:///Users/litany/WorkBuddy/清理器/internal/run/runner.go#L560-L712)、[run-readable-log.py](file:///Users/litany/WorkBuddy/清理器/scripts/run-readable-log.py#L597-L600)）：
   - **现象**：全量测试中 `TestAnUnansweredRequestIsNotAskedAgain` 失败，原因是 `Planner.CreateRulePlan` 开启 `SkipPending` 后直接跳过未决对象并计入 `ReasonAwaitingRequestCheck`，但 `Runner.quarantineGrants` 与 `purgeDue` 未把跳过的数量累加到 `UnknownHeld`（`review_unknown_held_total`）。
   - **修复**：在 `quarantineGrants`、`purgeDue` 与 `run-readable-log.py`（取 `review_summary.review_unknown_held` 与 `run.review_unknown_held_total` 较大值）同步补全计数。
3. **凭证基名全词干识别防护**（[classify.go](file:///Users/litany/WorkBuddy/清理器/internal/assess/classify.go#L70-L72)、[observation_rounds_test.go](file:///Users/litany/WorkBuddy/清理器/internal/assess/observation_rounds_test.go#L131-L138)）：
   - **现象**：真实沙箱首轮观察中，合成哨兵 `aws_credentials` 未命中原先的精确白名单名，导致被送往 `jev-1.14.0` 发起 1 次 HTTP 调用并消耗 725 input tokens。
   - **修复**：在 `ClassifyName` 增加 `strings.Contains(name, "credential")`，使 `aws_credentials` 等含 `credential` 词干文件在只读评估、规划与执行前直接归类为 `CatCredential`（0 次模型调用）。
4. **保留（`kept`）条目结构化判断与单层 token 回填**（[run-readable-log.py](file:///Users/litany/WorkBuddy/清理器/scripts/run-readable-log.py#L195-L204)）：
   - **现象**：无 `intent` 审计载荷的 `kept` 项在 `latest-readable.json` 中缺失 `jev_decision`、`minimax_consulted` 与单层 JEV token 统计。
   - **修复**：从 `review_explanation` 枚举结构与已结算（严格限定 `cost_status == "usage_recorded"` 且单次尝试）的 `review` 事件安全回填字段。
5. **`latest-readable.md` 消除单层拦截的矛盾文案**（[run-readable-log.py](file:///Users/litany/WorkBuddy/清理器/scripts/run-readable-log.py#L806-L814)、[test_run_readable_log.py](file:///Users/litany/WorkBuddy/清理器/scripts/test_run_readable_log.py#L372-L407)）：
   - **现象**：第一层 JEV 正常返回 `keep` 时因未调用 MiniMax，渲染层误按 `attempts > 0` 输出「综合结论 `keep`；已发出请求但未取得结果」。
   - **修复**：仅在 `model_error_recorded`、MiniMax 已发送但未返回有效判定、或 `reason` 为 `review_unknown`/`review_unavailable` 时提示「已发出请求但未取得结果」，否则输出 `MiniMax 未被调用`。

### 5.2 验证与真实双模型沙箱实测汇总

| 验证 / 实测项 | 执行命令 / 场景 | 结果与关键指标 |
|---|---|---|
| 全量无缓存单测 & 竞态回归 | `go vet ./...`、`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/fsops ./internal/assess`、`go test ./... -count=1`、`go test -race ./... -count=1` | 修复 #1–#2 后 **全部 33 个有测 Go 包**（含 `cmd/dustkeeper`、`internal/store`、`test/integration`、`test/release`）双向全绿，**0 DATA RACE**；修改 #3–#5 后复跑 `go vet ./...`、跨平台 `vet`、受影响 Go 包 `-race -count=1`（`assess`/`plan`/`execute`/`run`/`cleanup-report`/`debugrun`、`test/integration -run TestR18`）及 Python `unittest`（**37/37**）全绿 |
| 本机发布制品构建与校验 | `./build/build.sh --release` + `shasum -a 256 -c SHA256SUMS`（`classify.go` 修复后重刷） | 产出 `dist/dustkeeper-1.0.0-phase1-darwin-arm64`，SHA-256 `4d181c1c6047ddd809a251d3297b93cc138e90c2f4cc5f74207c9cd4a8f2b966`，`arm64` Mach-O / 临时签名 / `SHA256SUMS` 全通过；内置 `review-smoke` 记 `smoke_qr=ok`、`mixed_batch="deleted=1 observed=1 kept=1"`、`restored_bytes_match=yes` |
| 真实双模型评测 `review-eval` | `jev-1.14.0`（经 `127.0.0.1:7897`）+ `MiniMax-M3.1-Flash`，`-max-cases 2 -execute` | 默认 `90/90`：`zh-disposable-build-cache`（JEV `0.79/0.86` 未过阈值但双方均有 `usage` 记录→保守 `insufficient`）+ `zh-unique-bill`（JEV `keep 0.97`→0 MiniMax）；校准 `70/80`：`2/2` 全命中（`mismatches=0, abstentions=0`），`usage` 含 `cache_read_input_tokens` 与 `thinking_tokens` 均正常严格解析 |
| 只读真实模型诊断 `debug --execute` | `--open-files --advisory --execute`（含 `old_cache_dump.tmp`、进程打开的 `held_open.log`、`chflags uchg` `immutable_note.tmp`、`id_ed25519`） | 3 个保护/占用/凭证哨兵均保持 `keep` 且 **0 模型调用**；仅 `old_cache_dump.tmp` 进入模型评审（`model_calls=2`），输出 `advisory_only`，`simulated_cleaned_files=0` |
| 隔离沙箱两轮无人值守全链路（复验） | 修复后基于最新 `--release` 二进制，两轮 `run-scheduled-log.py`（`70/80`、`batch_max_attempts=4`）+ `collect-run-data.py` | 第 1 轮（`exit 0`, `http_attempts=4`）：`aws_credentials` 在规划前被 `PROTECTED` 零调用拦截，`build-cache-45d.tmp`（41B）经 JEV `0.830/0.890` + MiniMax `allow` 确认删除（`594ms + 19035ms`），`old-export.zip`（22B）经 JEV `insufficient` + MiniMax `deny (still_needed)` 安全保留（`652ms + 12558ms`），`cost_unknown=0`；第 2 轮（`exit 0`, `http_attempts=0`, `reserved_millicny=0` 退回预留）：`candidate_counts={"KEPT_RECENTLY":1,"PROTECTED":1}`，账本 `used_millicny=10/1000`，`health.json` `healthy=true`，生成 `0600` 诊断包 `11535` 字节 |

### 5.3 本机环境观察提示（未自动改动，需单独授权）

- **本机代理端口变化**：当前宿主机 `scutil --proxy` 生效代理为 **`127.0.0.1:7897`**（旧配置的 `127.0.0.1:7890` 端口未监听）；外部现有 `~/.dustkeeper-review/config.json` 与 `~/Library/Application Support/Dustkeeper-Managed/config/config.json` 仍写着 `127.0.0.1:7890`，按边界约定本轮未修改，后续如需在宿主直接联调或运行，请先确认并将对应配置中的 `jev_proxy_url` 更新为当前可用代理端口。
- **宿主机磁盘余量**：宿主机 `/System/Volumes/Data` 当前余量约 `73.7–77 GiB`（约 `8%` 空闲，接近 `min_free_percent: 5`/`10` 边界），真机运行若低于配置下限会自动进入 `DISK_PRESSURE`（暂停新隔离并仅放行已授权到期清空或确认的直接删除类别）。
- **仍待显式授权事项**：将本次二进制更新安装到宿主实机、`Dustkeeper-Managed` 解除暂停及日常运行调度、Linux 容器正式构建（`./build/build-linux-release.sh`）与 `elidli` 部署、以及 `git commit` / `push`。
