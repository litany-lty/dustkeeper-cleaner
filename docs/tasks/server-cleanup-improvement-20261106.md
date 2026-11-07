# 服务器清理整改交付说明（对应 2026-11-06 方案）

本文件回答方案 `dustkeeper-server-cleanup-improvement-plan-20261106.md`
（`/Users/litany/Documents/Codex/2026-11-06/new-chat/outputs/`）的七个问题，并逐条登记第 11 节 V1–V11
的证据位置。**只写实际发生的事**：哪些改在代码里、哪些只是操作办法、哪些没有验证。

---

## 1. 七个问题的直接回答

### Q1 哪些已修复、哪些只是操作办法、哪些仍未验证

| 类别 | 内容 |
|---|---|
| **已修复（代码）** | P0-01 预算分层与"提高候选数不买时间"；P0-02 失败分类（预算/超时/授权/供应商/传输/响应分开）；P0-03 未答复请求持久化并跨轮阻止再付费 + 费用未知状态；P0-04 部分完成的文案与"未调用/未记录"不再混说；P1-02 结果页与可读日志补请求事实、失败原因、恢复信息、体积与展示上限 |
| **已实现（新增能力，未在真实机器上跑过）** | P1-01 `retention plan` + `internal/retention`：按完整资源集合识别"最新一套"并列出例外 |
| **只是操作办法** | 本次实例里的两个旧包（MiniMax 超时包 `c18148b3…/application.jar`、预算耗尽包 `0296e3fb…/application.jar`）**仍按方案 §12 处理**：原位保留、规则显式排除、先核查请求与费用。本轮没有为它们补发任何请求，也没有放宽规则去"清掉"它们 |
| **职责合同（文档，非代码）** | P1-03 三类职责边界写进 [运维手册 8.5](../operations.md)：磁盘诊断是只读分析；系统/应用日志维护与 service 变更都不属于本程序的动作集合，也不由本程序记账 |
| **明确未做** | P2-01（空间回收不达预期/增量清理）未实现；本文件与 `docs` 都不声称已支持。方案 §10.1 的"不自动提交、推送、替换二进制、改服务器配置"按原样遵守：本轮**没有**替换服务器上的二进制、**没有**在 elidli 上部署、**没有**产生真实模型调用 |
| **未验证** | V11（真实隔离对象恢复）；Linux/systemd 下的长期定时运行；真实账单金额；真实双模型端点在新预算参数下的表现 |

### Q2 全局预算耗尽、供应商超时、有效 keep 在状态 / 调度 / 日志上如何区分

| | 全局（或批次）预算耗尽 | 请求已发出、没有答复 | 有效 keep（模型说保留） |
|---|---|---|---|
| 是否发出请求 | **否**（`http_attempts=0`，`cost_status=not_sent`） | 是（`http_attempts ≥ 1`，`cost_status=unknown`） | 视缓存，可能是 0 |
| 决策 reason | `not_started_budget` | `review_unknown`（同一对象下次为 `awaiting_request_check`） | `review_unavailable` 之外的 keep 原因 |
| 结果码 / 退出码 | `BUDGET_EXHAUSTED` / 9 | `MODEL_UNAVAILABLE` / 8 | `KEPT` / 0 |
| 调度影响 | 项级 `Control`：run 预算用完 → `stop_run`（不再进下一个 scope）；批次用完 → `stop_scope` | `stop_scope`：同一 scope 不再继续，避免继续为必然失败的问题付费 | `continue`：不影响后续对象 |
| 持久化状态 | 无新表；keep 事件里带 `review.failure=batch_budget/run_budget` | `review_unknown` 表一行（未解决），跨轮生效 | 无额外表；keep 事件带 `review.failure/cost_status` |
| 聚合计数 | 结果页 `review_not_started`、`run` 结尾 `reviews not started: N` | 结果页 `review_unknown_held` / `review_failed_kept` / `cost_unknown`；`run` 结尾 `unanswered earlier requests not repeated: N` | `reviewed` |
| 可读日志 | 「审核情况」："未开始（预算用完，未发出请求）：N 个，不产生请求费用" | 「审核情况」："请求未取得结果（文件未动）：N 个" + "费用未知（不能按 0 计）：N 个" | 「审核情况」："已取得判断：N 个"；逐候选仍有 JEV/MiniMax 结论与门禁 |

一句话：**"没花钱"和"花了钱没答案"与"问了、答案是保留"是三件不同的事**，它们的退出码、停止范围、
是否跨轮阻止重问、以及日志里那句话都不一样。

### Q3 同一请求费用未知时，为什么下一轮不会自动再次付费

1. 请求发出且没拿到答复时，`execute.noteUnknownRequest` 先写一行 `review_unknown`（对象、动作、scope、
   路径、身份与 token、尝试次数、失败类别、`cost_status=unknown`），**再**写 keep 记录。中途崩溃只可能丢后者，
   不会丢掉"这笔钱可能已经花了"。
2. 下一轮在处理同一个候选、发起审核**之前**先查 `heldUnknownRequest`：按 `scope_id + 路径 + 动作` 查未解决行
   （不是按 item id——重新计划会新建 item），再比对身份与 token。命中则直接记 `awaiting_request_check` 并保留，
   一次 HTTP 都不发。
3. 这条记录只有人能解除：`dustkeeper review unknown resolve --item … --action … --note … --confirm`。
   缺 `--confirm` 或缺 `--note` 都返回 `NEEDS_INPUT`（exit 2）。
4. 改规则、重新扫描、重装二进制都不清它；只有 `resolve` 写 `resolved_ns`。清空状态库能达到"不再阻止"的效果，
   但那等于销毁"付过费、没答复"的事实，不是本程序推荐的做法。

文件被改动过（内容哈希变化）视为另一个问题：旧行不阻止新请求，但它仍然留在库里，费用未知的事实不被覆盖。

### Q4 "最新一套"的证据是什么；例外有哪些；引用变化如何让旧计划失效

- **证据是声明式清单，不是目录列表**：每个应用登记发布集合（`published_at`、`published`、`complete`、
  `active`、`restore_in_progress`）、每个集合的全部组件、以及活动引用。判定顺序：活动引用 → 最新**合格**
  集合 → 进行中恢复 → 明确保护 → 未解决的不确定。
- **例外都带理由码**：`current_reference`（仍被引用）、`latest_qualified_release`、`restore_in_progress`、
  `explicit_protection`、`unresolved_review_or_operation`、`incomplete_release_kept_for_inspection`
  （不合格的新上传整套留着待查）、`component_still_referenced`。
- **"不合格"不因为新就优先**：缺组件的集合不能当最新版用，也不能因此把旧的完整集合删掉；此时
  `goal_met=false`，命令仍以 0 退出，把"目标没达成"当结论打印，不当作故障重试。
- **引用变化如何失效**：`retention plan` 只是提案，产物不落库、不带授权；真正执行仍走
  `rule preview` → `plan create` 的既有绑定（规则摘要、scope 根身份、对象身份）。引用变了就要重跑
  `retention plan` 并重新计划；旧计划会以 `GRANT_REQUIRED`（exit 4）或计划过期被拒绝，不会拿着旧结论删文件。

### Q5 打开哪一个日志文件就能看到完成量、剩余对象、判断依据与下一步

`latest-readable.md`（`run-readable-log.py` 每次真实 `run` 后原子更新，同时按 `run_id` 保留最近 10 轮快照，
目录总量上限 20 MiB）。它按顺序回答：

1. 「本轮概览」：扫描/候选/已删除/待删除/保留、删除的逻辑大小（**不冒充**磁盘回收量）、磁盘可用空间前后；
   部分完成时明确写"已删除 N 个文件……不能理解为整轮零操作"。
2. 「审核情况」：已取得判断、未开始、请求未取得结果、上次未知未重问、费用未知、本窗口 HTTP 尝试次数。
3. 「失败与下一步」：结果码、原命令说明、失败阶段、`next_steps`。
4. 「逐候选判断」：每个候选一条——动作（永久删除 / 移入隔离区 / 未移动）、依据（reason 文案）、
   最早可清空时间、恢复命令、JEV/MiniMax 结论与门禁数值、请求事实（发出几次、费用状态）、分段耗时。

没有可读日志时（例如只跑了 `run --json`），同一份事实在结果页 `<state>/reports/latest.md` 里：
`review_summary` 六个计数 + 每条 entry 的 `review_failure / http_attempts / cost_status / review_state`。
两者读的是同一个 `report.Page`，不各自算一遍。

### Q6 隔离对象如何恢复、何时最早可清空；哪些有真实证据、哪些没有

- 恢复：`dustkeeper quarantine restore --object <item_id>`（结果页/可读日志里给的就是 item id，不是 operation id）。
  目标已存在时不覆盖，返回冲突并保持两边原样。
- 最早可清空：来自对象记录的 `purge_after_ns`（观察期下限与规则保留期取大者）。可读日志写作"最早清空：
  <时间>（这是最早可清空时间，不是清空执行时间）"。
- **有证据**：本地受控用例覆盖恢复冲突、对账矩阵、隔离区对象身份校验（既有用例，本轮未改语义）；
  本轮新增用例覆盖"超时后已完成动作保留、剩余对象原位不动"。
- **未验证**：V11 要求对**真实隔离对象**执行恢复。这属于真实文件变更，本轮未获授权，因此**没有执行**，
  也就没有任何真实恢复证据。本次实例里那个 `obj_732daefa6f9e852f4519109308eb33fd`
  （最早清空 2026-11-13 17:17:07，Asia/Shanghai）仍是"已隔离、可恢复、未恢复"。

### Q7 实际运行的制品/脚本/配置对应哪一版；二进制能力与脚本层能力是否被准确区分

| 层 | 本轮状态 |
|---|---|
| 核心二进制 | **没有重新发布，也没有替换服务器上的文件**。改动只在源码分支里；任何要使用 `review_unknown` 表、新退出码语义、`retention plan` 的实例，必须换成包含迁移 `004.sql` 的这一版构件 |
| 状态库 | 迁移版本 4（新增 `review_unknown` 表 + 唯一索引）。旧库由 `init`/首次运行按 `PRAGMA user_version` 升级；**升级后旧二进制无法解释新表**，回退需要连状态库一起回到升级前的备份 |
| 脚本层（`scripts/run-readable-log.py`） | `schema_version=2`：新增 `summary.review`、`artifact_schema`、`listed/total/omitted_entries`、逐候选 `attempts/page_cost_status/page_review_state`、动作与恢复信息。旧页面（缺这些字段）显示"旧制品未记录"，不伪造 0 |
| 配置 | `runtime.run_soft_seconds` 默认 60 → **300**；新增 `review.batch_max_candidates / batch_seconds / batch_max_attempts / reserve_seconds`；不一致的配置在加载时被**拒绝**而不是钳制 |
| 真实运行 | 本轮没有任何真实 `run`、没有真实模型请求、没有部署。因此"新参数在真实端点下的表现"没有证据 |

脚本层能力 ≠ 二进制能力：可读日志能把**已经记录在页面/事件里**的事实整理好，但它无法补出二进制没有记录的
请求时序或费用状态；页面缺字段时它只写"旧制品未记录"。

---

## 2. 问题清单逐条状态

| 编号 | 状态 | 代码入口 | 证据 |
|---|---|---|---|
| P0-01 审核总预算与批量配置不匹配 | 已修复 | `internal/review/budget.go`、`settings.go`、`execute/review.go`、`config/config.go` | `TestABatchThatCannotFitTheNextCandidateDoesNotStartIt`、`TestARunThatExhaustedItsTaskAllowanceStopsSending`、`TestAShortTaskSliceStopsBeforeSendingAndSaysSo`、`TestBatchBudgetStopsBeforeSending` |
| P0-02 超时/预算/不可用被折叠成 `MODEL_UNAVAILABLE` | 已修复 | `review/facts.go`、`review/clients.go`、`outcome/outcome.go`、`execute/keep.go` | `TestFailureKindsMatchTheOutcomePackage`、`TestReviewTimeoutKeepsCompletedWorkAndStopsTheBatch`（区分 exit 8 与 exit 9） |
| P0-03 模型失败不进缓存、费用未知无持久化 | 已修复 | `execute/keep.go`、`store/review_unknown.go`、`migrations/004.sql` | `TestAnUnansweredRequestKeepsItsAttemptsAndUnknownCost`、`TestAnUnansweredRequestIsNotAskedAgain` |
| P0-04 部分完成文案、`LLMConsulted` 被清空 | 已修复 | `execute/apply.go`、`run/runner.go`、`report/*`、`scripts/run-readable-log.py` | `TestReviewTimeoutKeepsCompletedWorkAndStopsTheBatch`；脚本用例断言"部分完成……不能理解为整轮零操作" |
| P1-01 按完整资源集合识别最新一份 | 已实现（新命令） | `internal/retention/plan.go`、`cmd/dustkeeper/cmd_retention.go` | `internal/retention/plan_test.go` 五个用例 |
| P1-02 日志缺请求时序/隔离依据/恢复信息、需 schema 与上限 | 已修复 | `report/collect.go`、`report/render.go`、`scripts/run-readable-log.py` | `scripts/test_run_readable_log.py`（schema 2、20 MiB、200 条 + 总数/省略、恢复命令与最早清空） |
| P1-03 三类职责合同 | 文档 | `docs/operations.md` 8.5 | 本文件 Q1 |
| P2-01 空间回收不达预期/增量清理 | 未做 | — | 不宣传已支持 |

---

## 3. 验收表与证据位置

| 编号 | 证据 | 位置 |
|---|---|---|
| V1 已完成 10 个删除后供应商超时 | 通过：`Purged==10`、`FailureCode==MODEL_UNAVAILABLE`、剩余对象原位、`review_unknown` 行记 attempts 与 cost=unknown | `test/integration/review_budget_test.go` `TestReviewTimeoutKeepsCompletedWorkAndStopsTheBatch` |
| V2 预算在进入第二 scope 前耗尽 | 通过：`judge.Calls()==1`、`Control==stop_run`、该 scope 不报 `MODEL_UNAVAILABLE` | 同上 `TestBatchBudgetStopsBeforeSending` |
| V3 失败候选进入下一轮 / 进程重启 | 通过：第二轮不发新请求、文件未动、记录仍在；`resolve` 后才会再问 | 同上 `TestAnUnansweredRequestIsNotAskedAgain`；`internal/review/budget_test.go` |
| V4 供应商处理了但客户端没收到 | 通过：attempts=2、`FailureTimeout`、`cost_status=unknown`、verdict keep、reason `review_unknown` | `internal/review/budget_test.go` `TestAnUnansweredRequestKeepsItsAttemptsAndUnknownCost` |
| V5 双模型路径接近批次截止 | 通过：装不下下一候选时 `batch_budget`、attempts=0、cost=not_sent、供应商只被调一次 | `internal/review/budget_test.go` `TestABatchThatCannotFitTheNextCandidateDoesNotStartIt` |
| V6 最新上传不完整、旧集合完整且有引用 | 通过：保留旧完整集合 + 保留失败上传供检查，`GoalMet=false` 并说明 | `internal/retention/plan_test.go` `TestAnIncompleteNewestUploadKeepsThePreviousCompleteSet` |
| V7 恢复集组件版本号不同 | 通过：按集合整体保留，不按最大版本号 | 同上 `TestARestoreSetIsKeptWholeAcrossDifferentComponentVersions` |
| V8 直删不足后进入隔离的日志可追溯 | 通过（脚本层）：逐候选「动作」「最早清空（不是执行时间）」「`quarantine restore --object <id>`」；示例见用例夹具 | `scripts/test_run_readable_log.py` |
| V9 超时/取消/非 JSON/缺页面/旧格式 | 通过：非 JSON 输出 → `INVALID_OUTPUT` 且非零退出；失败轮仍写本轮可确认信息，不混入上一轮 | 同上（`DUSTKEEPER_FAKE_INVALID`、`DUSTKEEPER_FAKE_FAIL` 两个分支） |
| V10 latest 与历史快照同时出现 | 通过：总量取自页面 `total`，展示条数另计，`omitted_entries` 单列；隔离不计入"删除逻辑大小" | 同上（`total_entries=4 / listed=3 / omitted=1`，`deleted_logical_bytes=18 B`） |
| V11 真实隔离对象恢复 | **未验证**（未获授权，未执行） | — |

---

## 4. 行为变化（部署前需要知道）

1. **退出码 9 的含义变宽**：批次预算用完但已有对象处理完成时，也从 `MODEL_UNAVAILABLE`(8) 改为
   `PARTIAL`/`BUDGET_EXHAUSTED`(9)。定时任务里"8 才算故障"的告警规则要相应调整。
2. **`run_soft_seconds` 默认 300**（原 60）。显式写了更小值、又装不下 `2 × timeout_seconds + reserve_seconds`
   的配置会在加载时被拒绝——这是有意的：宁可启动失败，也不要"第二段请求永远被丢弃却记成供应商故障"。
3. **新增迁移 004**：升级后旧二进制无法处理新表，回退要连状态库一起回滚。
4. **新增两条命令**：`review unknown list|resolve`、`retention plan --manifest`。
5. **可读日志 schema_version=2**：读取方按版本分支；旧页面字段缺失时显示"旧制品未记录"。

---

## 5. 没有做的事（如实登记）

- 没有替换服务器上的二进制，没有部署到 elidli，没有重启任何服务。
- 没有产生任何真实模型调用，因此没有新账单；本次实例两条旧包的费用仍然未知。
- 没有对真实隔离对象执行恢复（V11）。
- 没有新增定时任务、没有做长期运行验证、没有做批量付费评测。
- 没有跑全量回归之外的额外压测；验证是 `go vet ./...`、`go test ./...` 与
  `python3 scripts/test_run_readable_log.py`。

---

## 6. 本轮实现与本地验证范围（接手核对用）

这一节给 [后续执行计划](dustkeeper-next-steps-20261106.md) §12 用：它引用的"本轮实现与本地验证范围"
就是这里。**本工作区的基线是 `8e46da5`（schema 4），不是计划里那个 `abd6cfc`（schema 5）候选**；两份
工作不是同一批提交，下面的清单只描述本工作区真实存在的东西。

### 6.1 实现入口

| 层次 | 文件 |
|---|---|
| 预算与批次 | `internal/review/budget.go`、`settings.go`、`facts.go`；`internal/config/config.go` |
| 请求事实与失败分类 | `internal/review/clients.go`、`types.go`、`outcome/outcome.go` |
| 未答复请求持久化 | `migrations/004.sql`、`internal/store/review_unknown.go` |
| 执行与停止范围 | `internal/execute/{apply,keep,purge,quarantine,review,protect}.go`、`internal/run/runner.go` |
| 两条新命令 | `cmd/dustkeeper/cmd_review.go`、`cmd_retention.go`、`review.go`、`main.go` |
| 保留策略 | `internal/retention/plan.go` |
| 结果页与可读日志 | `internal/report/{collect,render}.go`、`scripts/run-readable-log.py` |
| 运维说明 | `docs/operations.md` §3.1、§8.2–8.6、§9.1.1、§11 |

### 6.2 本地验证（可执行、可复现）

- `go build ./...`、`go vet ./...`、`go test ./...`（含 `cmd/dustkeeper`、`internal/review`、
  `internal/retention`、`test/integration`）通过。
- `python3 scripts/test_run_readable_log.py` 通过；`README` 里的包装器调用方式与实际参数一致。
- 定向反证：`internal/review/budget_test.go`（预算分两层、两次尝试预留、超时保留已调用事实）、
  `test/integration/review_budget_test.go`（部分完成、跨轮不重问、预算耗尽不发送请求）、
  `internal/retention/plan_test.go`（不完整最新集、跨版本恢复集）。
- 本工作区 `main` 的 push CI（run `42214748312`）在 `linux-amd64` 与 `darwin-arm64` 两个 job 均为
  success。**这是 8e46da5 的门禁结果，不是 `abd6cfc` 候选的**；候选从未推送，也没有 PR。

### 6.3 接手时发现的真实缺陷（本工作区已修）

1. `review unknown list` / `resolve` 从未调用 `a.openStore()`，真实二进制上会 nil pointer panic。
   现由 `cmd/dustkeeper/cmd_review.go` 显式打开状态库，并加 `cmd_review_test.go` 覆盖：无实例时返回
   `INVALID_ARGUMENT`、有实例时列出行、`--confirm` 与 `--note` 缺一返回 `NEEDS_INPUT`、resolve 后
   `--all` 仍能看到历史行。
2. `CheckStart` 允许只剩 1 次 HTTP 尝试就启动候选，双模型路径的第二段必然被丢弃。现按计划 §5.3
   预留 2 次尝试额度，`batch_max_attempts: 1` 合法但不启动任何审核；`budget_test.go` 有定向反证。

### 6.4 本工作区不具备、不能照抄的能力

`migrations/005.sql` / `internal/store/upgrade.go`（schema 3/4 → 5 显式升级）、
`cmd/dustkeeper/review_diag.go`（关联 ID 与诊断字段）、`scripts/collect-run-data.py`（只读 ZIP 采集）、
请求前 `dispatch_pending` 准备记录与结算事务、到期清空复用未知请求检查——这些都属于计划 §2.2 描述的
**另一份候选**，本工作区没有对应代码。升级、部署、采集步骤在拿到那份候选前都不执行。
