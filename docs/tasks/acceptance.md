# 验收记录

## 当前状态（分层填写）

> 本文件是**唯一继续维护的验收入口**；§1–§4 是核对与运行事实的填写区，§5 是 2026-11-08 对执行文档
> §4–§12 的逐条交叉验证映射（**B 阶段完成时点的历史快照**，不是第二份结论），§5.0A 是 2026-11-09
> C/D/E 阶段对 §9–§12 的执行回填。
> 执行依据：2026-11-08 的《Dustkeeper：修复、升级与运行验收执行文档》（本机交接文档，仓库外，不随制品分发）。
>
> **顶部「当前状态」表已就地回填**；带「截至整改完成」「未获授权 / 未执行 / 不适用」字样的段落是
> 2026-11-08 B 阶段完成时点的历史快照，按执行文档 §12 保留历史事实、不逐格改写为「已执行」。
> 以下表为最新结论；11-09 午后受控模型首跑见紧随本表的续办记录。§1、§2 与 §5.0A 保留此前阶段的回填事实。

| 可报告的状态 | 本轮结论 | 事实 / 缺口 |
|---|---|---|
| 代码修复完成 | **是** | P0 `scope rebind`、P1 `review dispatch resolve` + `006.sql`/schema 6、P2 正式路径 `request_timings` 已提交 `88097e4438b9f3af1366d54fd8a28c994b23f66a` 并推送 `main`；CI run `42507420327` 双平台 success；全量 `go test ./...`、`-race`、25 项 Python 用例、原生 release 构建冒烟通过。 |
| 新版本已安装 | **是** | 停 `com.dustkeeper.managed`、一致备份（30 文件）、替换二进制、显式 `init --json` 得 `from=3 / to=6 / upgraded=true`；升级前后 events/grants/items/observations/plans/rules/scopes/protections/purge_members/scan_queue 计数、scope 身份、授权行与 items 行**完全一致**，唯一变化是 `schema_version 3 → 6` 与三张空表。 |
| 身份恢复完成 | **是** | 真实 `downloads` 只读预览 digest `sha256:81ef65b1…` 确认后重绑定：generation `1 → 2`、protection_revision `1 → 2`、`status paused`、审计事件 `ev_abda156d…`；6 个隔离对象 `restore_chain_valid=true`、payload 与备份字节一致；`doctor` 六项全绿、`scope ok=true`；旧 grant 收窄为 `needs_reapproval`，旧 plan 保留。 |
| 手动清理可用 | **审核运行路径通过；实际移动尚未验证** | 21:53 受控复验 `dustkeeper exit=0`，外层 `run_97ab0c9bdf4dea76a4050b6cd916eac2` / 结果页 `run_9e2c94efb59490dd503f664240d1591e`。JEV 416ms、MiniMax 5815ms，均成功，各 1 请求；候选因 `move_safety_unknown` 保留，隔离/永久删除 0、`cost_unknown=0`。旧首轮鉴权失败仍保留，累计模型请求 4。 |
| 有限范围自动运行通过 | **已恢复调度；自然触发验收待观察** | 21:54 原 label `com.dustkeeper.managed` 已加载到受控 Python 入口及明确的 Managed 环境，Hour 19 / Minute 30；无 RunAtLoad / KeepAlive / StartInterval、无 kickstart。回读 `state=not running` / `runs=0`，4 次自然触发仍为 0；下一次计划 2026-11-10 19:30（Asia/Shanghai）。 |
| 待批准 / 阻塞 | **无当前执行阻塞；后续验收待实际发生** | 用户「继续」后，主控直接接续已批准的恢复、复验和调度操作，未消耗 WorkBuddy 积分。实例 `paused=false`、`pending_ops=0`、downloads active、原 6 个隔离对象仍在；guard `next_round_allowed=true`。尚未证明真实文件移动和 4 次自然触发，不以退出 0 或调度加载代替这些证据。 |

### 2026-11-09 21:54 续办：双模型复验通过，19:30 调度恢复

- 用户回复「继续」后，主控接续此前已经批准的本机操作，未切换模型、端点或账号配置，未使用 WorkBuddy 积分。先回读确认账本只有旧失败轮、实例暂停、调度未加载，随后备份账本并通过现有 `fee` / `recover` 入口处理。恢复标记只覆盖 round001，原 `aborted`、2 请求和错误事件未改。
- round002 为**手动复验**，不是自然触发：正常 `resume` 后通过 `finite-run.py run --kind manual` 执行一次。`dustkeeper_exit=0` / `healthy=true`，JEV `ok` 416ms、MiniMax `ok` 5815ms，各 1 请求；本轮没有重试。结果页与外层 run ID 不相同，以上表分别记录。
- 候选 `ApsaraDB-CA-Chain.zip`（248440 B）因 `move_safety_unknown / 无法确认移动是否安全` 原位保留。审核 1 件，`review_failed_kept=0`、`review_unknown_held=0`、`cost_unknown=0`；隔离/永久删除均为 0。实例回读 `paused=false`、`pending_ops=0`、downloads active、隔离对象数仍为 6、到期清空 0。**本轮证明审核及保留路径正常，不证明文件移动已验收**。
- 本轮运行前官方只读套餐查询返回 HTTP 200 / status 0，general 区间及周剩余比例均为 100%。MiniMax 按订阅用量处理；JEV 与 MiniMax 混合 token 不冒称可精确拆分。账本保留本轮 0.01 元预算，占用加旧轮估算合计 **0.010245952 元**；再预留下一轮为 0.020245952 元，远低于 70 元。它们是估算与工程预算，**不是实际账单扣款**。
- 21:54 备份原 plist 后，仅把运行参数替换为 `/opt/homebrew/opt/python@3.14/bin/python3.14` + 本机 `finite-run.py run --kind scheduled`，保留原 Managed config/state 和日志路径、每日 19:30。`plutil` 通过，`launchctl bootstrap` 退出 0；加载回读确认参数、环境和 `com.apple.launchd.calendarinterval` 中 Hour 19 / Minute 30，`runs=0` / `last exit=(never exited)`，没有立即补跑或伪造自然触发。
- 运行证据：`~/.dustkeeper-closeout-20261109/finite-execution/runs/round-002/`（结果页、订阅回执、用量、耗时、退出码）；调度回读：同目录 `schedule-loaded-20261109.txt`。原 ledger/plist 备份与 `manual-recovery-round-001.json` 也在该私有目录。guard 回读 `stopped=false` / `next_round_allowed=true` / `conservative_checked_through_round=2`；累计 2 轮、4 次模型请求，旧失败 1 轮、正常复验 1 轮。
- 下一次计划触发为 **2026-11-10 19:30（Asia/Shanghai）**。后续 4 次自然触发及实际文件移动仍未观察，不勾选相应验收项。此次复用已完成的 8 项定向检查，未重跑全量测试、构建或安装二进制。

### 2026-11-09 17:23 续办（历史）：授权修订完成，真实复验待接续

- 用户在主会话 `call_6d4b3f86fad9456db69f40e9002a9aa2` 回复「不限制次数」，取消旧 10 次请求及派生总轮次限制；70 元新增费用、每轮最多隔离 1 文件 / 1 GiB、无永久删除、异常暂停均不变。后续 4 次自然触发是观察目标，不是已经完成的证据。
- 用户批准「改用当前已登录 MiniMax 账号的订阅 Key，只更新 Dustkeeper 私有副本」。私有文件仍 0600/nlink=1、无符号链接和 ACL；JEV 凭据及全局配置未改变。新 Key 对官方只读套餐接口返回 HTTP 200 / `base_resp.status_code=0`，general 行区间/周剩余比例均为 100%；这是套餐鉴权与用量证据，不是模型调用。脱敏回执：`~/.dustkeeper-closeout-20261109/finite-execution/subscription-check-after-key-update.json`。
- 唯一旧 `review_unknown` 已用现有命令追加真实处理说明；主控读回 `resolved_ns=1794213189115597000`。原 `failure=auth`、`cost_status=unknown` 和原始事件保留，未伪造发票或把未知费用写为 0；没有重复调用 `review dispatch resolve`。
- 本机 `finite-run.py` 已取消累计次数限制，保留显式 Managed 实例、每轮范围、真实异常暂停和账本历史。预算采用已知估算费用加每轮未释放的 0.01 元预留，发起前不得超过 70 元；该预留明确为工程保守占用，**不是实际扣款或 JEV 实测**。首轮 MiniMax 401 无用量贡献，732 输入可归属 JEV；按官方 $0.042/百万输入、输出免费和 8 元/美元预算折算，估算 0.000245952 元，非逐笔账单核销。成功混合用量轮次不冒称可精确拆分，保留预算占用。
- 定向检查：语法检查通过，`test_revision_step.py` **8 项通过（1.710s，exit 0）**；覆盖无逐轮人工费用命令的连续运行、缺预算预留阻断、零调用正常结束/已调用缺用量暂停、真实套餐字段提取、自然触发不虚报、旧轮恢复不放行未来未完成轮次。复用此前已通过项，未重跑全套、构建或安装；本机运维脚本改动不改变已安装 `88097e4` 二进制。
- 最新账本 seq=4：追加一次授权修订，原 seq 1–3 和 round001 `aborted` / 2 请求未改；`stopped=true`，已知费用估算 0.000245952 元、held=0。真实模型累计仍 2 次，只有 `round-001`；未解除实际暂停、未加载调度、未产生新文件动作。
- 17:21 已向 WorkBuddy 下发已批准的真实复验与恢复调度步骤，但应用因 Hy4 免费额度频控直接失败。待用户选择接续方式后执行：仅恢复已处理的 round1 → 一次受控手动复验 → 健康才恢复同一实例每日 19:30 → 观察后续 4 次自然触发。不得用手动运行、测试通过或仅加载调度替代自然触发验收。

### 2026-11-09 午后首跑记录（历史）：配置已就绪，旧 Key 鉴权失败

- 首次授权为最多 10 次模型请求（已由上文最新授权取消累计次数限制）、70 元新增费用，每轮最多隔离 1 文件 / 1 GiB，无永久删除；并明确 MiniMax 使用现有订阅 Key，消耗套餐/积分，不按普通 API Key 的按量现金费用处理，不购买或变更订阅。
- 本机 `review` 现为固定 `jev-1.14.0` / `MiniMax-M3.1-Flash`、1 候选 / 2 次尝试，`run_soft_seconds=300`，`model.enabled=false`。凭据文件权限 0600、目录 0700；正确实例环境下 `status` / `doctor` 通过。未输出或提交凭据。
- 有限执行入口为 `/Users/litany/.dustkeeper-closeout-20261109/finite-execution/finite-run.py`，账本为同目录 `ledger.json`，证据为 `runs/round-001/`。它是本次本机运维入口，不属于已安装的 `88097e4` 制品。WorkBuddy 的 26 项定向夹具通过；主线程静态复核了实例绑定、异常暂停、额度和轮次边界，未另跑全量检查。
- 模型首跑失败：JEV 1 次成功、MiniMax 1 次 HTTP 401；共 2 次请求，失败计入额度，2 次预留未释放。没有自动重试或替换候选，隔离/永久删除均为 0。外层与结果页 run ID 如上表，不混用。
- 有限账本 `stopped=true`、首轮 `aborted`、费用核对状态仍为 `unverified`；不能把初始金额字段 0 当作本次实际费用为 0。新增 `review_unknown` 保留未解决状态，真实 `pause.request` 存在，调度未加载。
- 主线程另做 1 次官方只读套餐查询，得到 2049；这不是模型调用。其依据为 [MiniMax 官方 M Plan FAQ](https://platform.minimax.cn/docs/m-plan/faq)。当前模型地址依据见 [官方模型调用文档](https://platform.minimax.cn/docs/guides/text-generation)。未用新域名发起模型请求。
- 当时等待有效订阅凭据或区域确认（已由上文私有 Key 更新和只读鉴权结果接续）。任何后续操作都必须保留本轮 2 次请求及失败证据，不重置预算，不把未来自然触发预报为完成。本次仅本地更新状态文档，未提交、推送或重跑发布门禁。

**发布与制品对应**：最终候选 `88097e4` → CI run `42507420327`（push main）→ `darwin-arm64` SHA-256
`6c556e5aa6b8ee01aad207367b736c17c699d66162e4b9bb31c76d52e4e9f0fd`、`linux-amd64`
`9e0521ea12eb7cbc72da014a4ec30756d0a146966ba71d5dfb673aa13d8a9cdf`，与同次 manifest / `SHA256SUMS`
及实机 `version --json`（schema 6 / commit `88097e4` / darwin/arm64）一一对应。旧 CI `42379326052` 与
`5be1e90`/schema 5 制品只证明旧代码，未用于安装。C/D/E 之后若再有提交，均为纯文档回填；即使 CI 因此
再次触发，也**不构成新的安装候选**，最终安装制品仍以 `88097e4` / run `42507420327` 为准。

**当前实例状态**：`/Users/litany/.local/bin/dustkeeper` 为 CI 制品（`88097e4` / schema 6），旧二进制另存
`~/.local/bin/dustkeeper.old-2cbf8cb` 及备份目录。真实库 schema 6、实例 ID `inst_4a8ad6d08c9ccf2b4bf7a180114834be`
不变、scope `downloads` generation 2 / `active`（首跑时由 `paused` 恢复）、全局暂停已解除、调度已通过受控入口恢复每日 19:30（21:54 回读，见上文）。
6 个 quarantined 对象与 6 份 manifest 仍在隔离区、字节未变；`owner.json` / `vault-bind.json` / manifest
仍固化旧 `dev-16777229` 标签作为历史证据，而库内根身份 / vault 身份 / items 的 captured identity 已重映射到
`dev-16777231`（当前 boot `F0B18583-…`）。`scope_mount_binding:downloads` 由新二进制 `OpenVerified` 在身份
比较通过后刷新为当前 boot/device——这是 P0 修复后 scope 可被重新打开的预期行为，**不是数据丢失或绕过验证**。
此前 `REVIEW_REQUIRED` 运行曾使 `items` 从 4 planned 变为 5 planned + 6 quarantined，`plans` 新增一条
`blocked`；这是该历史轮次的规划记录，不是文件动作，也不作为午后模型首跑的统计。最新结果以上表及本次轮次证据为准。

**完成口径**：可写代码修复、安装、身份恢复、真实双模型审核与保留路径通过、调度已恢复；**不可写「全部清理验收完成」**。实际移动和 4 次自然触发仍需各自证据，不得用
「所有测试通过」「程序退出 0」「定时任务已加载」替代。完整 C/D/E 回填见 §1、§2 与 §5.0A。

---

对应 [后续执行计划](dustkeeper-next-steps-20261106.md) §10–§11。C/D/E 阶段执行后，§2 已回填
本轮实际结论；仍未观察到的字段保留"待执行 / 未观察到"，不得留空后默认为通过，
也不得用 CI 结果替未部署的真实运行背书，更不得用一次 `REVIEW_REQUIRED` 的运行替有效清理背书。
CI 里的隔离/恢复与混合批次冒烟用的是假模型，不算真实模型调用。

**基线说明（重要）**：接手文档写的是 `abd6cfc`（`fix/closeout-review-observability`），那三次提交在本工作区
与远端都不存在。用户后续明确授权「以 8e46da5 重做」「推送 main」，因此本轮的实际基线是
`8e46da5` → `9aa6c4c` 主线，**不再是 `abd6cfc`**；文档 §2.2 的八条能力已在这条基线上重建，schema 已到 5。
差异与逐条实现见 [接手核对与重建记录](handover-20261107.md) B 段。验收记录中的"是否相对 `abd6cfc` 又有修复"
一栏不再适用，改为记录本轮主线提交。

---

## 1. 收口检查表（§10.1）

**第 1、2 项为 11-07 历史制品门禁证据，按原样保留；本轮 C/D 的两条新事实追加在原第 3 项之前，现为第 3、4 项。**
**截至 2026-11-09：第 1–4 项已勾选（1、2 为历史制品门禁，3 为 C 阶段发布，4 为 D 阶段备份 / 显式升级 / 状态核对）；
第 5–9 项仍未勾选。**
勾选制品门禁不等于"已安装"或"已真实运行"，勾选第 4 项也不等于清理验收通过。

- [x] 本轮主线提交已推送到 `main`、通过双平台原生 CI（用户改选"推送 main"，不另建 PR）。
      证据：`8e46da5..5be1e90`；CI run `42379326052`，`linux-amd64` / `darwin-arm64` 两 job 均 success。
- [x] 最终 main 的制品、commit、SHA-256、version 有完整对应关系。
      证据：两份 CI manifest 均 `commit=5be1e90` / `version=1.0.0-phase1`，SHA-256 与 `SHA256SUMS`
      及实测一致；darwin 制品 `version --json` 得 `schema=5`。**该历史制品当时未安装到任何主机**（本轮安装的是 `88097e4` 制品，见下一条）。
- [x] 本轮新代码 `88097e4` 已推送 `main`、通过双平台原生 CI（沿用用户已选定的"推送 main"方式）。
      证据：CI run `42507420327`，`test (ubuntu-latest, linux-amd64)` / `test (macos-latest, darwin-arm64)`
      均 success；`darwin-arm64` SHA-256 `6c556e5a…`、`linux-amd64` `9e0521ea…` 与同次 manifest /
      `SHA256SUMS` 及 `version --json`（schema 6）一致；已安装的是这份 CI 原生制品。
- [x] 一个明确范围的目标实例完成备份、显式升级和状态核对。
      证据：2026-11-09 停 `com.dustkeeper.managed` → 一致备份 30 文件（源库与备份 SHA-256 均为
      `47a4fef2…`）→ 替换二进制 → `init --json` 得 `from=3 / to=6 / upgraded=true` → 前后计数、scope
      身份、授权、items 行逐项一致 → `scope rebind` 只读预览 + digest 确认（generation 1→2、6 个对象
      `restore_chain_valid=true`、payload 与备份字节一致）→ `doctor` 六项全绿。详见 §2 与 §5.0A。
- [ ] 一次小范围真实运行有有效候选、结果可解释、现场与记录一致。**已执行一次真实 `run --json`
      （`run_id=run_a839fccd…` / 结果页 `run_07f127aa…`），但 `REVIEW_REQUIRED` 保留全部候选、
      0 文件动作，属 §11.2-5 的「本轮未验证清理动作」，不能勾选。**
- [ ] 正式路径的调度至少真实触发一次，后续 3 次预定触发均有记录。
- [ ] 日志与采集材料能够回答计划 §8.3 的六个问题。
- [ ] 没有未处理的数据状态异常、越界动作或阻断基本使用的错误。
- [ ] 验收记录准确写明未触达分支、未测指标和运行范围。

手动执行与调度相同的命令**不算**调度验收；`test/eval` 因无真实端点 skip 时单列为"未运行"，
不能解释为模型准确率验证。

---

## 2. 验收记录表（§11）

| 类别 | 记录 |
|---|---|
| 源码 | **11-07 历史制品对应的源码提交** `5be1e90b99847aff85348d3422358d18fdc9e0c4`；PR：不适用（用户改选推送 main）；相对 `abd6cfc` 的比较：不适用（基线已改）。**本轮最终候选** `88097e4438b9f3af1366d54fd8a28c994b23f66a`（2026-11-09 推送 main）；其后的提交均为纯文档回填，不改变最终安装候选 |
| CI | 历史：run `42379326052`（`5be1e90`，两平台 success）。**本轮：run `42507420327`（push main，`88097e4`）**，`test (ubuntu-latest, linux-amd64)` / `test (macos-latest, darwin-arm64)` 均 success；build/vet/test/race/release/制品校验/归档全绿。本机另跑全量 `go test ./...`、`-race`、25 项 Python 用例与 `build.sh --release`，全部通过。`test/eval` 无真实端点时的 skip 仍属"未运行" |
| 制品 | 历史：`5be1e90` / schema 5，darwin `5a7312e7…`、linux `0e38e493…`，**均未安装**。**本轮**：manifest `commit=88097e4` / `version=1.0.0-phase1` / `target=darwin-arm64`，darwin SHA-256 `6c556e5aa6b8ee01aad207367b736c17c699d66162e4b9bb31c76d52e4e9f0fd`、linux `9e0521ea12eb7cbc72da014a4ec30756d0a146966ba71d5dfb673aa13d8a9cdf`，与 `SHA256SUMS` 及实测一致；实机 `version --json` → schema **6** / commit `88097e4` / darwin/arm64 / SQLite 3.53.4。**darwin 制品已安装**，linux 只核对清单（本机不可执行） |
| 目标实例 | macOS darwin/arm64、账号 `litany` (uid 501)；二进制 `/Users/litany/.local/bin/dustkeeper`；config `/Users/litany/Library/Application Support/Dustkeeper-Managed/config`、state `…/state`；唯一 scope `downloads`（根 `/Users/litany/Downloads`，`managed_disposable`）；调度 `com.dustkeeper.managed`，每天 19:30。备份与证据目录 `/Users/litany/.dustkeeper-closeout-20261109/`（0700，清理范围之外） |
| 升级 | 旧 schema **3** → 新 **6**，`init --json` 报 `from=3 / to=6 / upgraded=true`、`config_created=false`。停机点：bootout 调度后、无进程/无锁、sidecar 为空、`pending_ops=0`。备份 30 文件，源库与备份 SHA-256 同为 `47a4fef2…`。前后逐项对照：events 12、grants 1、items 10、observations 1384、plans 1、rules 2、scopes 1、protections 0、purge_members 0、scan_queue 218 全部不变；scope ID / 根路径 / 实例 ID / generation / protection_revision / status / root_identity / vault_identity 全部不变；授权行与 items 行逐行一致。唯一变化：`schema_version 3→6` 与新增 3 张空表。`config.json`、`downloads-rule.json`、schedule 下的两个文件与备份逐字节一致 |
| 配置 | `run_soft_seconds=60`、`run_read_budget_bytes=1 GiB`、`max_auto_quarantine=100 文件/2 GiB`、`max_auto_purge=100 文件/1 GiB`、`min_free=512 MiB/5%`；`model.enabled=false`、**无 `review` 段**（模型名/端点为占位值），因此双模型门禁未配置；规则 `quarantine` / `*.dmg,*.zip` / 30 天 / 排除 `pdf/`、`*-000.zip`、`bill_overview*` / retention 30 天；通知沿用历史确认的 `none`。凭据值不写进文档。首跑前用 `rule enable --max-files 1 --max-bytes 1073741824` 把额度收窄到小批（只读预览 `enable_digest sha256:bbd1091…` 后确认），新 grant `grant_db722cf…` = `active`/`auto_quarantine`/1 文件/1 GiB，旧 grant 保留为 `needs_reapproval` |
| 手动首跑 | 外层命令 `run_id=run_a839fccd47f7187f8001354f4361f989`，结果页/scope 层 `run_id=run_07f127aa8ebdd1ec7e6ecd1a1d6bda55`（两者不同，按 §2.3 不擅自拼接）；原始 `exit=6`、`code=REVIEW_REQUIRED`、`status=blocked`、`control=stop_scope`；扫描 1390、`scan_epoch_complete=true`；`quarantined=0`、`purged=0`、`read_bytes=0`、HTTP 尝试 0、`cost_unknown=0`、无重复请求；磁盘可用空间 93502328832 → 93501603840（−724992 B，约 −708 KiB，来自日志/结果页写入，**不是释放数据块**）。结果页 `reports/result-run_07f127aa….json/.md`、`latest.json`、可读报告均已归档。**没有有效清理动作**，属 §11.2-5「本轮未验证清理动作」 |
| 调度 | `com.dustkeeper.managed` 于 2026-11-09 升级前 bootout，**至今仍卸载**；无自然日历触发、无对应 run ID。手动 `run` 不计入调度验收 |
| 后续观察 | 后续 3 次预定触发：无。§11.3-1 要求「仅在手动首跑满足要求且调度批准有效时」恢复调度，本轮首跑不满足，故未恢复；已观察轮次 0，无「下一触发时间」可记录 |
| 数据 | `evidence/run/dustkeeper-run-20261109.zip`（0600，5082 B，2 个源文件 / 26901 B，采集时刻 2026-11-09 02:12）；缺项：`logs/dustkeeper.jsonl.1/.2/.3`（可选轮转日志）明确标为 gap。原始 `run.stdout.json` / `run.stderr.txt`（空）/ `run.exit.txt=6`、结果页、结构化日志、`run.log`、可读报告、重绑定预览与确认输出均在 `evidence/` 下。**诊断 ZIP 不是一致性恢复备份** |
| 未验证 | 真实模型请求（0 次，配置缺失）、有效隔离/永久删除动作（0 次）、到期清空分支（due=0、无 purge 授权）、30 天观察期、跨启动 `dev-*` 行为的真实设备长期验证、完整调度 1+3 次自然触发。另记两处口径差异：① `run.stdout.json` 的 `review_not_started_total=0` 而结果页 `review_summary.review_not_started=1`；② 可读报告 `--run-json` 模式把退出码归一化为 `exit 1`，原始 CLI 退出码为 `6`（脚本既有设计，原始证据已单独保存）。两者不影响「本轮 0 文件动作」的结论，转维护核对 |
| 最终结论 | **源码可发布：是；新版本已安装：是；身份恢复完成：是；手动可运行（有效清理）：否；有限范围自动运行：否。** 阻塞为双模型审核配置与凭据缺失，不是授权；不得提前写"转维护"或"全部完成" |

**本轮实际结论（2026-11-09）**：

> 本次使用提交 `88097e4438b9f3af1366d54fd8a28c994b23f66a`、CI run `42507420327` 的
> `darwin-arm64` 制品（SHA-256 `6c556e5aa6b8ee01aad207367b736c17c699d66162e4b9bb31c76d52e4e9f0fd`），
> 在本机 `Dustkeeper-Managed` 实例完成一致备份、显式 `3 → 6` 升级、原路径目录身份重绑定和一次真实运行。
> 升级与重绑定前后历史数据逐项一致，6 个隔离对象与 manifest 保持原样。真实运行 `exit=6` /
> `REVIEW_REQUIRED`，0 文件动作、0 次 HTTP 尝试、0 费用；**调度未恢复，无自然触发记录**。
> 未验证范围：真实模型审核、有效隔离/永久删除、到期清空、30 天观察期、调度 1+3 次自然触发。
> 当前仍阻断项为**双模型审核配置与凭据缺失**（`review` 段、真实端点/模型名、第二层凭据），
> 结论为**停留在「身份恢复完成」，未进入「手动清理可用 / 有限范围自动运行通过」**。

---

## 3. 逐轮采集与交接材料（§8.1–§8.2）

逐轮使用不同输出文件名，避免 `latest` 被下一轮覆盖：

```sh
python3 /absolute/scripts/collect-run-data.py \
  --state-dir /absolute/confirmed/state \
  --output /absolute/private/dustkeeper-run-YYYYMMDD-HHMMSS.zip
```

> 脚本现在已在本工作区 `scripts/collect-run-data.py`（与二进制同一提交）。白名单只有四份标准 JSONL 日志与
> `reports/latest.json`，每份日志最多 2 MiB + 64 KiB、结果页最多 4 MiB，源文件总上限约 12.25 MiB；
> 输出为新建的 0600 ZIP，不覆盖已有文件；清单记录状态/大小/SHA-256/读取期间变化，缺失、拒绝、不可读、
> 超限都明确标成缺项。**ZIP 只用于分析，不是一致性恢复备份（备份见 `operations.md` §11 的四件套），
> 也不是供应商账本**；自定义 `result-dir` 不在白名单时，另存实际结果页并标注来源与 run ID。
> `run` 不支持 `--scope`，首跑前必须核对整个实例可能触达的 scope、pending、到期清空与授权范围。

同一私有目录保留：`acceptance.md`、`version.txt`、`release-manifest.txt`、`build-facts.txt`、
`SHA256SUMS`、`run-<run_id>.json`、`run-<run_id>.stderr.txt`、`run-<run_id>.exit.txt`、
`schedule-evidence.txt`、`dustkeeper-run-<timestamp>.zip`。逐轮 JSON / stderr / 退出码来自真实命令；
缺失字段标记"缺失"，不写成成功。

## 4. 结论只回答六个问题（§8.3）

1. 是否完成了实际清理（已删除 / 待删除 / 保留及原因分别统计；隔离不等于释放数据块）。
2. 慢在哪里（总轮次、JEV、MiniMax、连接与响应分阶段；样本少只报样本）。
3. 额度是否匹配工作量（候选、尝试、剩余时间、`not_started_budget`）。
4. 失败是否被正确处理（鉴权 / 供应商 / 传输 / 响应 / 超时 / 预算 / 本地拒绝）。
5. 是否存在自动重复请求（关联 run / item / review ID 与未解决记录；日志缺失不等于没发生）。
6. 是否还需要修代码（只有可定位、可复现、影响使用的缺陷进入修复；否则写"转维护"）。

HTTP 尝试次数不证明供应商收到或计费；缺账单写"费用未知"。

---

## 5. 交叉验证：执行文档 §4–§12 逐条映射（2026-11-08）

> 依据：仓库外的《Dustkeeper：修复、升级与运行验收执行文档》（2026-11-08）。本节把该文 §4–§12 的
> **全部 63 个 `- [ ]`**、三段最小验收（§5.3 / §6.4 / §7.3）、§9 发布纪律、§10.4 回退边界与 §12 交付标准
> 逐条落到事实层。上面的「当前状态」表仍是**唯一结论**，本节不新增第二份收口报告；执行文档原文未修改。
>
> **历史快照声明（2026-11-09）**：本节 §5.0–§5.11 记录的是 B 阶段完成时点（2026-11-08）的事实，其中
> 「未获授权 / 未执行 / 不适用」与 `B1`–`B6` 阻塞编号描述的是**当时**状态。用户随后给出全阶段批准，
> C/D/E1 已执行；按执行文档 §12，本节原文保留历史事实、不逐格改写，C/D/E 的逐条执行结果见 **§5.0A**。
> 最新结论仍以顶部「当前状态」表和 §2「本轮实际结论」为准。

### 5.0 状态口径与共享阻塞编号

> **B 阶段历史口径**（2026-11-08）：下面的「未执行」定义与 `B1`–`B6` 描述的是当时状态；
> 其现状逐条对照见 §5.0A A.1。

| 标记 | 含义 |
|---|---|
| 实测 | 本轮实际运行了只读检查、定向测试或构建，结果见 §5.10 |
| 静态 | 逐行核对本轮代码 / 文档差异，未对真实托管实例执行 |
| 旧证据 | 沿用本轮之前已取得的证据，已标注时间与适用边界 |
| 未执行 | C/D/E 阶段；本轮无具体授权，不执行、不冒充完成 |

共享阻塞（§5.7 / §5.8 各行引用）：

- `B1` 本轮新代码的提交 / 推送 / 新 CI 候选制品（commit、平台、schema 6、二进制 SHA-256、同次 manifest）未获授权；工作树仍未提交、未推送，无新制品。
- `B2` 停止 `com.dustkeeper.managed`（19:30 日历触发）并取得一致备份未获授权；调度单元仍在加载，未 bootout，未做备份。
- `B3` 替换 `~/.local/bin/dustkeeper`、对 `Dustkeeper-Managed` 执行 `init --json` 的 `3 → 6` 显式升级未获授权；本机仍 `2cbf8cb` / schema 3。
- `B4` 对原路径 `/Users/litany/Downloads` 提交 `scope rebind --confirm` 未获授权；6 个隔离对象未做真实重绑定。
- `B5` 解除全局暂停、真实模型调用与小范围 `run --json` 未获授权。
- `B6` 恢复调度、等待 1 次自然日历触发 + 后续 3 次观察未获授权。

### 5.0A C/D/E 阶段执行回填（2026-11-09）

> 用户于 2026-11-09 给出全阶段批准（「全部批准，开始完成任务」），覆盖执行文档 §1.1 列出的提交、推送、
> 正式安装、真实运行、解除暂停、重绑定、启用调度与小范围真实运行。本节把 §10 的 15 项、§11 的 14 项，
> 以及 §9 发布纪律与 §12 交付条款逐条落到事实层。**批准不等于配置已存在**：本机 `config.json` 缺 `review`
> 段与凭据，按执行文档 §1.2 未擅自补配置、未降低双模型门禁、未扩大范围、未把「批准」误读为「配置已就绪」。
> 最新结论仍以顶部「当前状态」表和 §2「本轮实际结论」为准，本节不新增第二份结论。

#### A.1 `B1`–`B6` 阻塞编号的现状对照

| 编号 | B 阶段历史状态（§5.0） | C/D/E 实际（2026-11-09） |
|---|---|---|
| `B1` | 新代码提交 / 推送 / 新 CI 候选制品未获授权 | **已完成**：`88097e4` 推送 `main`；CI run `42507420327` 双平台 success；manifest / `SHA256SUMS` / 实机 `version --json` 一致 |
| `B2` | 停调度 / 一致备份未获授权 | **已完成**：bootout `com.dustkeeper.managed`（未动其他服务）；库静止后 30 文件、0700 备份；源库与备份 SHA-256 同为 `47a4fef2…` |
| `B3` | 替换二进制 / `3 → 6` 显式升级未获授权 | **已完成**：旧二进制留副本；`init --json` 得 `from=3 / to=6 / upgraded=true`；升级前后逐项一致 |
| `B4` | 真实 `scope rebind --confirm` 未获授权 | **已完成**：只读预览 digest `sha256:81ef65b1…` 后确认；generation `1 → 2`、6 个对象保留、`doctor` 六项全绿 |
| `B5` | 解除暂停 / 真实模型调用 / 小范围 `run --json` 未获授权 | **部分完成**：已解除暂停并只跑一次 `run --json`；但缺 `review` 段与凭据，0 次模型调用、0 文件动作，属 §11.2-5「本轮未验证清理动作」。真实模型调用仍为 **0** |
| `B6` | 恢复调度 / 1 + 3 次自然触发未获授权 | **条件保护生效，未执行**：§11.3-1 要求首跑满足要求才恢复；本轮不满足，故调度保持卸载、全局暂停已恢复、观察轮次 0。这是纪律约束，不是遗漏，也不是授权不足 |

#### A.2 §9 验证与发布纪律

| 条目 | B 阶段 | C 阶段实际 |
|---|---|---|
| §9.1 每项修复先确认缺口、P0/P1/P2/文档分别定向验证 | 实测（§5.10） | 保持；提交前另跑正式门禁：`go build ./...`、`go vet ./...`、`go test ./... -count=1`、`go test -race ./... -count=1`、25 项 Python 用例、`./build/build.sh --release`，全绿 |
| §9.1 独立临时目录、不向真实配置写假端点 | 实测 | 保持；夹具仍用 `t.TempDir()`；真实实例安装的是 CI 原生制品，不是本地临时构建 |
| §9.1 全量 Go/race、双平台原生构建不因本文自动触发 | 未执行（符合纪律） | C 阶段正式发布门禁已执行并全绿；未做批量截图集，未做额外独立审查 |
| §9.2 五项候选制品纪律 | 未执行 · `B1` | **已满足**：新 commit `88097e4` + SHA-256；CI 由 push `main` 触发一次（run `42507420327`），未重复本地构建充当候选；两平台原生制品 + `release-manifest.txt` / `SHA256SUMS` / `build-facts.txt`；实机 `version --json` 与清单一致，不只看版本名 |

#### A.3 §10 本机升级与恢复（15/15 已执行）

| 条目 | 状态 | 实际证据 |
|---|---|---|
| §10.1-1 停止 `com.dustkeeper.managed`，不停止其他服务，先查状态不强杀 | 已执行 | `launchctl print` 查状态后 `bootout gui/501/com.dustkeeper.managed`；bootout 后 `print` 返回 `Could not find service` 即卸载证据；未强杀进程、未动其他 label |
| §10.1-2 保持全局暂停，确认无进程持锁、pending=0 | 已执行 | `pause --json` 得 `PAUSE_REQUESTED` / `newly_written=false`（既有 CLI 把「已暂停」记为 `blocked` / exit 6），marker 存在、`paused=true`；无进程、无锁、sidecar 为空、`pending_ops=0` |
| §10.1-3 清理范围外私有备份目录，保留 config / state + sidecar / 完整 `.dustkeeper/` / 旧二进制 / version / SHA-256 / plist | 已执行 | `/Users/litany/.dustkeeper-closeout-20261109/backup/`（0700，清理范围之外），30 文件，含 `backup-manifest.txt` |
| §10.1-4 凭据只在本机按原权限或更严格保存，不贴入报告或上传 | 已执行 | 完整 config 只在本机 0700 备份；文档与证据均不含凭据值、完整配置、提示词、文件正文、原始模型响应或带访问权限 URL |
| §10.1-5 库静止且旧进程退出后复制；非空 journal/WAL 保留现场；不删 sidecar | 已执行 | sidecar 为空，库静止后 `cp -p`；源库与备份 SHA-256 同为 `47a4fef2…`；未删 sidecar |
| §10.1-6 记录备份清单与摘要，确认 6 个隔离对象与 manifest/owner/anchor 仍在 | 已执行 | `backup-manifest.txt` + SHA-256；6 个 objects / 6 份 manifest / `owner.json` / `vault-bind.json` 逐项在备份中 |
| §10.2-1 核对最终候选制品与脚本，替换前保留旧二进制，不先清理 | 已执行 | 核对 `release-manifest.txt` / `SHA256SUMS` / `build-facts.txt` / `version --json` / `file`；旧二进制另存 `~/.local/bin/dustkeeper.old-2cbf8cb` 并进备份 |
| §10.2-2 显式指定 config/state 执行最终制品 `init --json`，记录 from/to/upgraded | 已执行 | `from=3 / to=6 / upgraded=true`、`config_created=false`；普通 `status` 未暗中迁移 |
| §10.2-3 比较实例 ID / scope ID / 根路径 / 规则授权保护 / observations / planned / quarantined / pending / 审核历史 | 已执行 | events 12、grants 1、items 10、observations 1384、plans 1、rules 2、scopes 1、protections 0、purge_members 0、scan_queue 218 全部不变；scope ID / 根路径 / 实例 ID / generation / protection_revision / status / root_identity / vault_identity 一致；授权行与 items 行逐行一致。唯一变化：`schema_version 3→6` 与新增 3 张空表 |
| §10.2-4 核对实际预算与阈值，不擅自降门槛或加额度；用当前验证器 | 已执行 | config 四文件与备份逐字节一致；未改软时限 / 读预算 / 自动隔离与清空额度 / `min_free` / 阈值；首跑范围收敛用的是既有 `rule enable` 机制 |
| §10.2-5 升级后仍保持调度关闭、全局暂停 | 已执行 | 升级与重绑定期间调度保持卸载；pause marker 未动；成功重绑定后 scope 落 `paused` |
| §10.3-1 用 P0 只读预览核对当前证据与备份一致 | 已执行 | digest `sha256:81ef65b1a04a84faaff4adfaebd786876ce742bb5b0fa20b8d9ccb7a082a4eaf`；`Changed=""`、`Weak=""`、仅 `RuntimeRelabeled`、`boot_changed=true`、6 个对象逐项 `restore_chain_valid=true`、`unresolvable=null`、`global_paused=true`、`pending_ops=0` |
| §10.3-2 提交授权覆盖的重绑定确认；再读身份 / generation / scope 状态 / 隔离对象关联 | 已执行 | generation `1 → 2`、protection_revision `1 → 2`、`status=paused`、审计事件 `ev_abda156d55edeab23853d86e3ddc156b`、objects `6`；库内 `captured_identity_json` 从 `dev-16777229` 重映射为 `dev-16777231`；`owner.json` / `vault-bind.json` / 6 份 manifest 仍保留旧标签作历史证据；6 个 payload 与备份字节一致；`doctor` 六项全绿、`scope ok=true` |
| §10.3-3 确认旧计划/授权未被放宽；重新预览并在批准范围内重新确认规则 | 已执行 | 旧 grant 收窄为 `needs_reapproval`，旧 plan 保留未删；`rule enable --max-files 1 --max-bytes 1073741824` 只读预览 `enable_digest sha256:bbd1091…` 后确认，新 grant `grant_db722cf04a4d096fff0005a8f6b0de22` = `active` / `auto_quarantine` / 1 文件 / 1 GiB，绑定新 generation 2 |
| §10.3-4 `doctor` / 只读检查通过后才讨论解除暂停与首跑；不用 `--accept-stale-hold` 代替 | 已执行 | 重绑定后 `doctor` 六项全绿才进入 E1；全程未使用 `--accept-stale-hold` |

`scope_mount_binding:downloads` 在只读预览前已刷新到当前 boot/device（`F0B18583-…` / `16777231`），
而备份中仍是旧值：这是新二进制 `OpenVerified` 在身份比较通过后刷新运行期挂载绑定的预期行为，
**不是数据丢失或绕过验证**（顶部「当前实例状态」已同步说明）。

#### A.4 §10.4 回退边界

B 阶段静态核对的三项约束已写入 `operations.md` §11，并在本次备份中执行：库静止后 `cp -p`、备份含完整
config / state + sidecar / `.dustkeeper/` / 旧二进制 / version JSON / SHA-256 / 实际 plist，权限 0700。
**仍未做回退演练**（本轮无新隔离 / 恢复 / 永久删除动作，未触发回退条件）；备份副本不是测试沙箱、
库内为绝对路径，此项如实保留，不写成「已验证可回退」。

#### A.5 §11 小范围真实运行与调度验收（14 项逐条）

| 条目 | 状态 | 实际证据 / 缺口 |
|---|---|---|
| §11.1-1 核对批准覆盖的类别/范围/最大文件数与字节/外发字段/费用额度/永久删除许可/通知 | 已执行 | 只读统计可匹配 28 个候选 / 7.07 GiB，宽于「小范围」；通知沿用历史确认的 `none`；无 purge 授权 |
| §11.1-2 重看整个实例 scope / 旧 planned / pending / 到期清空 / 授权；`run` 不支持 `--scope` | 已执行 | 升级后复核 scope / grants / items / plans / pending / `due=0`；未按 scope 过滤运行 |
| §11.1-3 批准范围窄于整轮触达范围时，用现有配置/规则机制收敛；不新增通用 scope 过滤 | 已执行 | `rule enable --max-files 1 --max-bytes 1073741824`；未新增通用过滤能力、未改 config |
| §11.1-4 有费用/请求未知记录先核查；不人为构造真实异常 | 已执行 | 首跑前无未结算 `review_dispatch`、无 `review_unknown`；首跑产生 1 条 `state=refused` / `attempts=0` / `http_status=0` / `cost_status=not_sent` / 已有 `settled_ns` 的**已结算** dispatch，不是未结算请求，故**未**调用 `review dispatch resolve`；未人为构造异常 |
| §11.2-1 已批准小批执行一次 `run --json`，保留 stdout / stderr / 退出码 / 结果页，不自动重跑 | 已执行 | 只跑一次；`run.stdout.json` / `run.stderr.txt`（空）/ `run.exit.txt=6` / 结果页 / `latest.json` / 结构化日志 / 可读报告均在 `evidence/run/` |
| §11.2-2 核对有效候选 / 模型调用事实 / 文件动作 / 隔离路径与恢复信息 / 结果页与日志一致性 | **已核对，无有效动作** | 扫描 1390、`scan_epoch_complete=true`；`REVIEW_REQUIRED` 保留全部候选，`quarantined=0` / `purged=0` / `read_bytes=0`、HTTP 尝试 0、`cost_unknown=0`；无新增隔离路径或恢复信息；两处口径差异见 A.7-4 |
| §11.2-3 预算不足与模型超时分开报告；失败原因明确；同一未解决请求不重复调用 | 已核对 | 无预算不足、无模型超时；失败原因明确为 `REVIEW_REQUIRED: retained: configure dual review before any quarantine or deletion`；0 次重复请求 |
| §11.2-4 分别报告删除逻辑字节 / 隔离字节 / 磁盘实际可用空间变化 | 已执行 | 删除逻辑字节 0、隔离字节 0；可用空间 `93502328832 → 93501603840`（−724992 B ≈ −708 KiB）来自日志 / 结果页写入，**不是释放数据块** |
| §11.2-5 无有效候选或仍因 held 跳过只记「本轮未验证清理动作」，不擅自扩大范围 | 已执行 | 如实记为「本轮未验证清理动作」；未扩大范围、未重跑、未改配置降低门禁；已恢复全局暂停 |
| §11.3-1 仅首跑满足且调度批准有效时恢复同一 label / 同一实例路径，沿用 19:30 不增频 | **未执行（条件不成立）** | 首跑 `REVIEW_REQUIRED` 不满足「手动首跑满足要求」；`com.dustkeeper.managed` 保持卸载，plist 未改，`StartCalendarInterval Hour=19 / Minute=30` 未变 |
| §11.3-2 等待一次自然日历触发 + 后续 3 次记录；`kickstart` / 连续运行不算 | 未执行 | 依赖 §11.3-1；已观察轮次 0；未用 `kickstart` 或连续 `run` 冒充 |
| §11.3-3 触发跨数日；会话提前结束只记已观察轮次与下一触发时间，不提前写完成，不另建常驻任务 | 未执行 | 已记「已观察轮次 0」；调度未恢复，无「下一触发时间」可记录；未创建常驻任务或自动化 |
| §11.3-4 每轮记录触发时间 / 实际 commit+schema / run ID / 扫描动作跳过原因 / 退出码 / 诊断 | 未执行 | 无自然触发轮次；手动首跑的对应字段已在 §2 与 `evidence/run/` 记录，**不计入调度验收** |
| §11.3-5 有失败只处理具体失败，不自动提高预算 / 扩大范围 / 重发未知费用请求 / 清空队列 | 已遵守 | 未提高预算、未扩大范围、未重发、未清空队列；仅对缺失配置做只读核对并如实登记 |

#### A.6 §12 交付内容与分层状态重新判定

| §12.1 交付项 | B 阶段判定 | 当前判定 |
|---|---|---|
| 1 代码差异及对应问题，只含 P0/P1/P2 与必要文档 | 已交付 | 已交付（`88097e4`；§5.2 / §5.3 / §5.4 逐条对应） |
| 2 最小验证及结果，区分实测 / 旧证据 / 静态 / 未执行 | 已交付 | 已交付（§5.10 + C 阶段正式门禁全绿） |
| 3 若已发布：commit / CI / 平台 / schema / SHA-256 对照 | 不适用（`B1`） | **已交付**（§1、§2、顶部「发布与制品对应」） |
| 4 若已升级：实例路径 / 备份位置 / 升级前后对照 / 重绑定核查 / 未处理对象 | 不适用（`B3`） | **已交付**（§2「目标实例 / 升级」、A.3、顶部「当前实例状态」；未处理对象为仍留在隔离区的 6 个 quarantined） |
| 5 若已实跑：退出码 / run ID / 动作与跳过原因 / 请求与计费核查 / 观察轮次 | 不适用（`B5` / `B6`） | **部分交付**：前四项已在 §2「手动首跑」记录；观察轮次为 **0**，不写成已完成 |
| 6 更新一份现有验收记录；未获授权部分列为待执行 | 已交付 | 已交付：仍只维护本文件；B 阶段的「未获授权」已被全阶段批准解除，剩余阻塞是**客观配置缺失**与 **E2 条件未成立** |

**§12.2 分层状态**：顶部「当前状态」表已就地回填 —— 「代码修复完成 / 新版本已安装 / 身份恢复完成 = 是」，
「手动清理可用 / 有限范围自动运行通过 = 否」，「待批准 / 阻塞 = E2 + 模型配置」。
未用「所有测试通过」「程序退出 0」「定时任务已加载」替代后两层，也未写「全部完成」。

#### A.7 仍未覆盖 / 转维护核对项

1. §5.11-1 的两部分证据：**(a) 已补齐** —— 在真实 `dev-16777231` 上做过只读预览并逐项核对 6 份 manifest /
   owner / anchor / payload，`restore_chain_valid=true`、payload 与备份字节一致；**(b) 仍未取证** —— 重绑定确认后
   按既有授权流程恢复一个真实对象。为不改动 6 个历史对象、不把取证变成清理动作，未执行 `quarantine restore`；
   夹具中的等价证据仍见 §5.3。此项如实保留，不写成「恢复链已端到端验证」。
2. 长期未覆盖：真实模型请求 0、有效隔离 / 永久删除 0、到期清空分支（`due=0`、无 purge 授权）、30 天观察期、
   跨启动 `dev-*` 行为的真实设备长期验证。
3. E2 调度 1 + 3 次自然触发未发生；`com.dustkeeper.managed` 仍卸载，无「下一触发时间」可记录。
4. 两处口径差异转维护核对：① `run.stdout.json` 的 `review_not_started_total=0` 而结果页
   `review_summary.review_not_started=1`；② 可读报告 `--run-json` 模式把退出码归一化为 `exit 1`，
   原始 CLI 退出码为 `6`（脚本既有设计，原始证据已单独保存）。两者不影响「0 文件动作 / 0 HTTP 尝试 / 0 费用」。
5. 当前阻塞不是执行授权，而是客观配置：`review` 段、JEV / MiniMax 真实端点与模型名、阈值、凭据，
   以及模型外发字段与费用额度确认。补齐后需重新核对范围、（必要时）重新 `rule enable`、执行一次有效
   `run --json`；**只有首跑满足要求后**，才按 §11.3-1 恢复调度并观察 1 + 3 次自然触发。

### 5.1 §4 阶段 A：一次性核对（5/5 完成，全部只读）

| 条目 | 状态 | 证据 / 缺口 |
|---|---|---|
| §4-1 项目约定 + `ub-mode/SKILL.md` | 实测 | 已读；按 TDD、最小改动、不自动提交/推送/部署/清理、不 stash/reset、保留用户改动执行 |
| §4-2 工作树 / HEAD / 工具 / 新改动交集 | 实测 | 起始工作树干净；`main` @ `722cece9d07d…`；go 1.26.1 darwin/arm64、Python 3.13.12；本轮 31 个已改 + 3 个新文件，均属 P0/P1/P2 与文档，无外来改动被覆盖 |
| §4-3 plist / 二进制版本 / 实例状态 / 最近跳过 | 实测 | `launchctl print gui/$(id -u)/com.dustkeeper.managed`：`~/.local/bin/dustkeeper run`、`Hour=19`/`Minute=30`、`state=not running`、`last exit=0`、`runs=3`；本机二进制 `2cbf8cb`；未倾倒完整日志 / 配置 / 凭据 |
| §4-4 scope / 隔离对象 / 计划 / 授权 / 保护 / pending / 全局暂停计数 | 实测 | schema 3；`events=12`、`grants=1`、`items=10`（6 quarantined / 4 planned）、`observations=1384`、`plans=1`、`rules=2`、`scopes=1`、`protections=0`、`purge_members=0`、`scan_queue=218`；`downloads` 为 `held`，全局暂停有效 |
| §4-5 只读比较根与隔离区旧/新身份，缺失字段如实标记 | 实测 | 旧 `dev-16777229` → 当前 `dev-16777231`，boot session `C0FAFF7C-…` 已变；根 inode `278384`、birth time、owner `501`、文件系统、vault nonce `vault_b3ca…`、anchor tag、6 份 manifest 未变；`owner.json` / `vault-bind.json` / mount binding 仍固化旧标签。未证实项不推断 |

旧制品证据（CI run `42379326052`、`5be1e90`、darwin SHA-256 `5a7312e7…`、schema 5）标为**旧证据**，只证明旧代码，见 §2；不为本轮背书。

### 5.2 §5 P0：保留历史的目录身份恢复

**§5.2 实施要求（11/11）**

| 条目 | 状态 | 证据 / 缺口 |
|---|---|---|
| 1 独立临时实例复现 held 且现有命令无法恢复 | 实测 | `cmd/dustkeeper/scope_rebind_test.go::rebindFixture`：真实临时实例 + 真实隔离对象 + 规则/计划/授权，只改写持久化半边；`TestScopeRebindPreviewIsReadOnlyAndResumeCannotRecover` 证明 `resume --accept-stale-hold` 无法恢复。未用真实 Downloads 做夹具 |
| 2 查清是运行期编号变化还是真实替换；证据不足不宣称同一对象 | 实测 + 静态 | `internal/fsops/platform_darwin.go` 注释标明 `dev-<st_dev>` 是运行期标签；`internal/scope/identity.go` 新增 `LegacyVolumeLabel` / `LegacyDevice` / `RootBootChanged` / `RuntimeRelabeled`。真实实例成因**未宣称已证实**，保持 `held`，P1/P2 独立完成 |
| 3 实现 `scope rebind --id <scope>`：只读预览 + digest 确认 | 实测 + 静态 | `cmd/dustkeeper/cmd_scope.go` 新增 `scopeRebind`；`internal/scope/rebind.go` 新增 `RebindPreviewFor` / `ConfirmRebind`。无 `--digest` 只读，确认需 `--digest --confirm --backup --schedule-stopped --note`。已安装旧二进制仍**没有**该命令 |
| 4 预览字段齐全，确认重验同一套事实，证据变化拒绝旧 digest | 实测 | 预览含 scope / 实例 / 原路径 / 新旧 generation / 身份差异 / 隔离对象逐个证据 / 计划 / 授权 / 保护 / pending / 全局暂停 / digest；`TestScopeRebindRefusesChangedEvidence/stale_digest` |
| 5 真实重绑定的前置门禁；只允许原路径 | 实测 + 静态 | 代码检查 `filepath.EvalSymlinks(p.Root) == p.Root`、实例锁、`ScopePendingOps`、备份目录、`--schedule-stopped`、`--note`、全局暂停；真实执行仍属 `B2`–`B4`，未发生 |
| 6 保留 scope ID / 历史 / 隔离内容 / manifest / 保护 / 审核历史；禁止批量替换 `dev-*` | 实测 + 静态 | 全仓 `grep` 无 `strings.Replace` / `ReplaceAll`；`RemapRuntimeLabel` 只解析结构化 `domain.FileIdentity` 并改写 `VolumeID`/`Device` 两个字段；`TestScopeRebindPreservesHistoryAndRestoresTheObject` 逐项比对 |
| 7 处理 vault / owner / anchor / objects 绑定与运行期挂载记录 | 静态 + 实测 | `internal/scope/vault.go`：`OpenVault` 的 legacy 分支用 `VaultIdentityUnder` / `AnchorIdentityUnder` 在旧标签下复算摘要才能放行，仍检查 privacy、owner、inode、scope、kind、nonce、anchor tag、same-device；`ApplyScopeRebind` 更新 `vault_identity_json` 与 `scope_mount_binding:*`。只改状态列不构成修复 |
| 8 旧计划/授权不得自动获得新的删除权限；不删旧计划 | 实测 | generation 与 protection_revision 各 +1；active grant 收窄为 `needs_reapproval`；`TestScopeRebindPreservesHistoryAndRestoresTheObject` 断言旧授权不可跨 generation 执行，旧 plan 仍在 |
| 9 逐项证明隔离对象记录仍指向原对象、恢复链有效，不扩大能力 | 实测 | `objectEvidence` 逐项核对对象存在、inode/birth/generation 可对应、manifest 存在且 `object_id`/`scope_id`/`instance_id` 匹配；同夹具里走既有 `quarantine restore` 取回**原字节** |
| 10 成功后保持全局暂停，scope 至多 `paused`，不启动模型/扫描/恢复/清理 | 实测 | `ApplyScopeRebind` 写 `status='paused'`，不动 pause marker；`TestScopeRebindPreservesHistoryAndRestoresTheObject` 断言状态；恢复入口未新增任何能力 |
| 11 多处写入的中断处理，单事务，失败保留 held，可继续 | 实测 | `store.ApplyScopeRebind` 单个 `s.write` 事务；新增 `TestHookBeforeRebindEvent`；`TestScopeRebindCommitBoundaryKeepsTheOldHeldState` 证明事件写失败整笔回滚、scope 保持旧 generation 与 `held`。未搭建通用迁移框架 |

**§5.2 末段根因约束**：未删除 `VolumeID` / `Device` 比较（静态核对 `CompareRootIdentity`）；同一次启动内 device 变化仍拒绝（`TestSameBootDeviceChangeIsStillRefused`）；inode/owner/文件系统/birth/generation/weak 证据仍严格（`TestBootRelabelStillChecksEveryPersistentFact`）；稳定卷标识仍跨启动严格比较（`TestStableVolumeIDIsComparedAcrossBoots`）；`operations.md` §3.2 明写「跨启动行为尚未在真实设备上长期验证，不能写成已彻底解决」。

**§5.3 最小验收（5/5）**

| 条目 | 状态 | 证据 |
|---|---|---|
| 1 独立夹具保留真实隔离对象与规则/计划，预览不改状态 | 实测 | `TestScopeRebindPreviewIsReadOnlyAndResumeCannotRecover` |
| 2 确认后 scope/对象历史保留；旧授权不能跨 generation | 实测 | `TestScopeRebindPreservesHistoryAndRestoresTheObject` |
| 3 原隔离对象按既有授权流程恢复，内容一致 | 实测 | 同上（夹具内 `quarantine restore` + 原字节比对） |
| 4 预览后身份再变 / owner-vault 不匹配 / 未完成文件操作时拒绝且不开放清理 | 实测 | `TestScopeRebindRefusesChangedEvidence` 四个子用例：`stale_digest`、`owner_marker_does_not_match_the_vault`、`unfinished_file_operation`、`stored_root_identity_changes_again`（本轮交叉验证补） |
| 5 数据库 + vault 多处写入覆盖一个真实提交边界 | 实测 | `TestScopeRebindCommitBoundaryKeepsTheOldHeldState`（`TestHookBeforeRebindEvent` 注入事件写失败） |

**§5.3 末段（B 阶段历史）**：真实实例 6 个隔离对象的恢复链**未能取证**，缺失证据与下一步见 §5.11；
未清空隔离区、未新建实例绕过。C/D 阶段已补齐只读预览部分，见 §5.0A A.7-1。

### 5.3 §6 P1：未结算审核请求的人工处理

**§6.2 语义约束**：缺 `--confirm` / 缺 note / 非法状态 / 未核查时拒绝、`not_sent` + `charged` 矛盾组合拒绝、只处理指定未结算 dispatch、重复确认不覆写、其他阻塞仍生效、下次运行仍过完整门禁 —— 全部由 `ValidateDispatchResolution` / `ResolveReviewDispatch` 与 §5.3 表列测试覆盖（实测）。**「执行者必须核对批准与剩余额度」是运维动作，本轮没有真实目标 dispatch，未执行；命令本身不扩大授权（静态核对）。**

**§6.3 持久化（6/6）**

| 条目 | 状态 | 证据 / 缺口 |
|---|---|---|
| 1 实例锁 + 事务内检查未结算，写 resolution 与独立人工事件，失败全回滚 | 实测 | `ResolveReviewDispatch`；`TestManualResolutionRollsBackWhenEventFails` |
| 2 默认 list / `OpenDispatchFor` 只把「pending 且无人工结论」视为未解决；`--all` 分开显示；可复制 dispatch ID | 实测 | `LEFT JOIN`；`TestManualResolutionLiftsOnlyThatDispatch`、`TestHeldUnknownRequestLiftsOnlyResolvedDispatch`、`TestReviewDispatchListShowsPendingRequests` |
| 3 原 `cost_status` / attempts / usage 不倒改，"准备时记录"与"人工核查后的事实"分开 | 实测 | `TestExplicitUpgradePreservesExistingData`、`TestManualResolutionKeepsFirstRecordAndClosedRows`；CLI 文案见 `cmd_review.go` |
| 4 人工处理事件不增加审核次数 / attempts / token / 文件动作数 | 实测 | `TestReviewDispatchResolveLiftsOnlyOneAndKeepsHistory`、`TestManualResolutionLeavesReviewUnknownIndependent`；事件只追加 `review_dispatch_resolution` |
| 5 新增 `006.sql`、更新 schema 版本与嵌入；不改 `005.sql`；显式升级支持 3/4/5；普通命令拒绝版本不一致且不迁移 | 静态 + 实测 | `migrations/006.sql`（追加式表）、`embed.go` `Schema006` / `CurrentVersion=6` / `Steps` 第 6 步；`git diff -- migrations/005.sql` 为空；`TestExplicitUpgradeFromThreeFourAndFive`（from_3/4/5 全通过）、`cmd_init_upgrade_test.go` 证明 `status` 不暗中迁移 |
| 6 同步 `upgrade.go` 目标版本说明、**完整性检查**及现有升级测试；不沿用「升级到 5」 | 实测 + 静态 | 目标版本注释改为 3/4/5 → 6；`upgrade_test.go` 三个升级用例已更新；**本轮交叉验证补上完整性检查**：`proveOurDatabase` 保持迁移前的 5 张核心表要求（不能要求 schema-6 新表，否则 3/4/5 全部无法升级），新增迁移后 `proveUpgradedSchema` 校验 `review_dispatch_resolution` 确实落地，并由 `TestProveUpgradedSchemaRequiresSchemaSixTable` 先红后绿验证。全仓 `*.go` 已无「升级到 5 / schema 5」文案 |

**§6.4 最小验证（4/4）**

| 条目 | 状态 | 证据 |
|---|---|---|
| 1 pending 阻止再调用；正确核查后仅该记录解锁，历史可查 | 实测 | `TestManualResolutionLiftsOnlyThatDispatch`、`TestReviewDispatchResolveLiftsOnlyOneAndKeepsHistory` |
| 2 缺确认/说明/未核实状态不改库；重复不覆盖；其他未解决记录继续阻止 | 实测 | `TestReviewDispatchResolveRejectsUncheckedInputWithoutWriting`、`TestManualResolutionRejectsUncheckedInput`、`TestManualResolutionKeepsFirstRecordAndClosedRows` |
| 3 事务写事件失败时核查记录也不落地 | 实测 | `TestManualResolutionRollsBackWhenEventFails` |
| 4 显式升级保留旧 scope/隔离/未知请求数据；普通 status 不暗中迁移 | 实测 | `TestExplicitUpgradePreservesExistingData`、`TestExplicitUpgradeRefusesUnknownOrUncleanState`、`cmd_init_upgrade_test.go` |

**边界**：未在真实实例制造中断请求，未调用付费接口；沿用 `cmd_review_test.go` / `review_dispatch_test.go` / `dispatch_test.go` / `upgrade_test.go` 与临时实例/假模型设施。

### 5.4 §7 P2：正式运行的分段耗时

**§7.2 实施要求（7/7）**

| 条目 | 状态 | 证据 |
|---|---|---|
| 1 时序采集与 `ReviewBoth` 解耦；保持 JEV→MiniMax 短路/门槛/超时/预算 | 实测 + 静态 | `internal/review/clients.go::HTTPJudge.Review` 无条件建立 `timingBatch`，`defer` 写回 `Decision.RequestTimings`；`ReviewBoth` 仍只控制 debug 第二意见。`TestHTTPJudgeKeepDoesNotCallMiniMax` 等既有用例未回归 |
| 2 复用受限 `RequestTiming`；只记录实际请求；未发送不生成 0ms 记录 | 实测 + 静态 | `transport.go` 未改动；`TestProductionTimingBudgetRefusalHasNoTimings` |
| 3 `stages_ms` 是事件时间点；只在两个事件点都存在时相减，缺失显示未采集/不适用，不补零 | 实测 | `scripts/run-readable-log.py` 的 `STAGE_PAIRS` / `stage_durations` / `optional_ms`；`test_timing_helpers_and_uncalled_model` |
| 4 时序沿结算事件 → 结果 JSON → 运行日志 → 可读报告传递，按真实 review ID 关联；成功/超时/失败均记录 | 实测 | `dispatch.go::withRequestTimings`、`review_diag.go`、`report/collect.go::reviewTimings`、`render.go::timingSummary`；`TestSettleReviewCarriesTimingsToDiagAndEvent`、`TestCollectCorrelatesTimingsByReviewID` |
| 5 不扩展为全量网络日志；禁止 URL/代理/头/正文/文件内容/凭据；轮转与大小限制不变 | 实测 + 静态 | 三个测试文件断言 forbidden payload 不出现；`git diff -- scripts/collect-run-data.py` 为空（白名单 4 份 JSONL + `latest.json`，2 MiB+64 KiB / 4 MiB / 总约 12.25 MiB 上限未改） |
| 6 人读报告写明哪个模型耗时、是否未调用、最后观测阶段；旧数据写"未记录" | 实测 | `render.go::timingSummary` + `run-readable-log.py::review_summary`；`TestUnknownRequestFactsAreNotZeroFilled`、Python 用例的旧行断言 |
| 7 `collect-run-data.py` 白名单与总大小限制保持现状 | 静态 | `git diff` 为空，上条同时核对 |

**§7.3 最小验证（5/5）**

| 条目 | 状态 | 证据 |
|---|---|---|
| 1 正式模式 JEV 保留：只有 JEV 时序，MiniMax 调用数 0 | 实测 | `TestProductionTimingRecordsOnlyJEVOnKeep` |
| 2 正式模式两层成功：恰好两次调用，按 review ID 看到两层时序 | 实测 | `TestProductionTimingRecordsBothLayers` |
| 3 第二层超时：保留 JEV 事实与 MiniMax 失败阶段、总耗时，不自动重试 | 实测 | `TestProductionTimingMiniMaxTimeoutKeepsJEVFact` |
| 4 预算拒绝：无请求时序、无付费请求；旧结果字段缺失的人读展示不填零 | 实测 | `TestProductionTimingBudgetRefusalHasNoTimings`、Python 旧行断言 |
| 5 事件→JSON→可读报告不丢 `stages_ms`，内容无敏感字段 | 实测 | `TestCollectCorrelatesTimingsByReviewID`（`stages_ms` 留在结果 JSON）、`cmd/dustkeeper/review_diag_test.go`、`test_timing_helpers_and_uncalled_model`（V11 + 泄漏断言） |

只测受影响分支；未做真实模型延迟评测或大批次性能测试。

### 5.5 §8 文档更新范围（5/5 + 禁止项）

| 条目 | 状态 | 证据 |
|---|---|---|
| 1 `README.md` 顶部四种状态；FINAL-1.0 归入历史 | 静态 | 顶部新增「当前状态（2026-11-08，四种状态分开看）」表；FINAL-1.0 段标明历史 |
| 2 `operations.md` 的 scope 身份恢复 / dispatch 人工核查 / 时序说明 / 实际 schema 与升级步骤 | 静态 | §3.1（schema 6、3/4/5→6、迁移前后完整性检查）、§3.2（`scope rebind`）、§8.3（人工核查）、§8.7（`request_timings`）；本轮同步修正 §3.2 回退措辞与 §11 的本机 launchd label 指向 |
| 3 备份口径统一：系统 `sqlite3 .backup` 不作通用命令 | 静态 | §11 改为库静止后 `cp -p` 数据库 + sidecar、完整 config、`.dustkeeper/`、旧二进制、version JSON、SHA-256、实际 plist；显式说明系统 sqlite3 未在所有本机环境验证；诊断 ZIP 不是一致恢复备份 |
| 4 `acceptance.md` 顶部标当前状态；复用它记录新候选与最终运行事实 | 静态 | 本文件顶部「当前状态」表 + 本节；§2 的三行在 B 阶段留待回填，C/D/E 后已就地回填（§2 / §5.0A）；未新增竞争报告 |
| 5 历史接手文档保留历史，只在必要处指向当前验收记录 | 静态 | `handover-20261107.md` / `dustkeeper-next-steps-20261106.md` 开头各加「历史快照」说明，旧提交、schema 5、SHA-256、测试证据原样保留 |
| 禁止项：不自动导出 PDF / 图片 / 截图集 / 仪表盘 | 静态 | 本轮新增文件仅 `scope_rebind_test.go`、`rebind.go`、`006.sql`；无导出物 |

### 5.6 §9 验证与发布纪律

| 条目 | 状态 | 证据 / 缺口 |
|---|---|---|
| §9.1 每项修复先确认证据缺口，再选现有或最小新用例；不靠全量测试掩盖根因 | 实测 | 本轮新增用例均对应具体缺口（P0 四项 + 追加的 identity 再变、P1 人工核查、P2 时序、schema-6 完整性检查先红后绿） |
| §9.1 P0/P1/P2/文档分别定向验证 | 实测 | 见 §5.10 命令清单；P0 夹具、P1 store/CLI/升级、P2 transport→报告、文档只核对本次差异 |
| §9.1 独立临时目录与实例，不向真实配置写假端点 | 实测 + 静态 | 全部 `t.TempDir()`；未修改 `~/Library/Application Support/Dustkeeper-Managed/`、未改 plist、未改 `~/Downloads/.dustkeeper/` |
| §9.1 全量 Go/race、批量截图、额外独立审查不因本文自动触发 | 未执行（符合纪律） | 只跑了受影响 7 个包的完整测试 + `go build ./...` 编译核对；**未**跑全量 `go test ./...`、**未**跑 `-race`、**未**做发布门禁 / 双平台原生构建 / 截图 / 独立审查。正式发布阶段才执行项目现有门禁 |
| §9.2 五项候选制品纪律（新 commit / SHA、CI 触发方式、不重复构建、原生制品 + manifest、不能只看版本名） | 未执行 · `B1` | 本轮无新提交、无推送、无新 CI、无新制品；旧 CI 证据按「旧证据」标注，不冒充本轮发布 |

### 5.7 §10 获准后的本机升级与恢复（15/15 未执行）

> **B 阶段历史快照**；下表「未执行」指 2026-11-08 当时状态。C/D 阶段这 15 项已逐条执行，见 §5.0A A.3。

| 条目 | 状态 | 阻塞 |
|---|---|---|
| §10.1-1 停止 `com.dustkeeper.managed`，不停止其他服务，先查状态不强杀 | 未执行 | `B2` |
| §10.1-2 保持全局暂停，确认无进程持锁、pending=0 | 未执行 | `B2`（阶段 A 只读确认过当前暂停与 pending 状态，不等于已执行停机） |
| §10.1-3 清理范围外私有备份目录，保留 config / state + sidecar / 完整 `.dustkeeper/` / 旧二进制 / version / SHA-256 / plist | 未执行 | `B2` |
| §10.1-4 凭据只在本机按原权限或更严格保存，不贴入报告或上传 | 未执行 | `B2` |
| §10.1-5 库静止且旧进程退出后复制；非空 journal/WAL 保留现场；不删 sidecar | 未执行 | `B2` |
| §10.1-6 记录备份清单与摘要，确认 6 个隔离对象与 manifest/owner/anchor 仍在 | 未执行 | `B2` |
| §10.2-1 核对最终候选制品与脚本，替换前保留旧二进制，不先清理 | 未执行 | `B1` + `B3` |
| §10.2-2 显式指定 config/state 执行最终制品 `init --json`，记录 from/to/upgraded（应为 3→6） | 未执行 | `B3` |
| §10.2-3 比较实例 ID / scope ID / 根路径 / 规则授权保护 / observations / planned / quarantined / pending / 审核历史 | 未执行 | `B3` |
| §10.2-4 核对实际任务/批次/单请求预算与阈值，不擅自降门槛或加额度；用当前验证器 | 未执行 | `B3` |
| §10.2-5 升级后仍保持调度关闭、全局暂停 | 未执行 | `B2` + `B3` |
| §10.3-1 用 P0 只读预览核对当前证据与备份一致 | 未执行 | `B4`（接口已实现并定向验证，未在真实实例执行） |
| §10.3-2 提交授权覆盖的重绑定确认；再读身份 / generation / scope 状态 / 隔离对象关联 | 未执行 | `B4` |
| §10.3-3 确认旧计划/授权未被放宽；重新预览并在批准范围内重新确认规则 | 未执行 | `B4` + `B5` |
| §10.3-4 `doctor` / 只读检查通过后才讨论解除暂停与首跑；不用 `--accept-stale-hold` 代替 | 未执行 | `B4` + `B5` |

**§10.4 回退边界**：三项约束均已在 `operations.md` §11 写明（升级后不能只换回旧二进制；无新文件动作时按一致备份恢复；有新隔离/恢复/永久删除时禁止盲目覆盖，永久删除不可撤销；备份副本不是测试沙箱，库里是绝对路径）。本轮只做静态核对，未做回退演练。

### 5.8 §11 小范围真实运行与调度验收（14/14 未执行）

> **B 阶段历史快照**；下表「未执行」指 2026-11-08 当时状态。E1 已执行、E2 因 §11.3-1 条件不成立未恢复调度，
> 14 项逐条现状见 §5.0A A.5。

| 条目 | 状态 | 阻塞 |
|---|---|---|
| §11.1-1 核对批准覆盖的类别/范围/最大文件数与字节/外发字段/费用额度/永久删除许可/通知 | 未执行 | `B5` |
| §11.1-2 重看整个实例 scope / 旧 planned / pending / 到期清空 / 授权；`run` 不支持 `--scope`，`--dry-run` 只列 pending | 未执行 | `B5` |
| §11.1-3 批准范围窄于整轮触达范围时，用现有配置/规则机制收敛；不新增通用 scope 过滤 | 未执行 | `B5` |
| §11.1-4 有费用/请求未知记录先核查；不人为构造真实异常 | 未执行 | `B5`（P1 接口已备好，真实实例未执行） |
| §11.2-1 已批准小批执行一次 `run --json`，保留 stdout / stderr / 退出码 / 结果页，不自动重跑 | 未执行 | `B5` |
| §11.2-2 核对有效候选 / 模型调用事实 / 文件动作 / 隔离路径与恢复信息 / 结果页与日志一致性 | 未执行 | `B5` |
| §11.2-3 预算不足与模型超时分开报告；失败原因明确；同一未解决请求不重复调用 | 未执行 | `B5` |
| §11.2-4 分别报告删除逻辑字节 / 隔离字节 / 磁盘实际可用空间变化 | 未执行 | `B5` |
| §11.2-5 无有效候选或仍因 held 跳过只记「本轮未验证清理动作」，不擅自扩大范围 | 未执行 | `B5` |
| §11.3-1 仅首跑满足且调度批准有效时恢复同一 label / 同一实例路径，沿用 19:30 不增频 | 未执行 | `B6` |
| §11.3-2 等待一次自然日历触发 + 后续 3 次记录；`kickstart` / 连续运行不算 | 未执行 | `B6` |
| §11.3-3 触发跨数日；会话提前结束只记已观察轮次与下一触发时间，不提前写完成，不另建常驻任务 | 未执行 | `B6` |
| §11.3-4 每轮记录触发时间 / 实际 commit+schema / run ID / 扫描动作跳过原因 / 退出码 / 诊断 | 未执行 | `B6` |
| §11.3-5 有失败只处理具体失败，不自动提高预算 / 扩大范围 / 重发未知费用请求 / 清空队列 | 未执行 | `B6` |

**§11 末段**：完整 30 天观察期、真实永久清空分支与全部故障恢复情形**仍未覆盖**；本轮没有模拟时钟或长期运行结果冒充。

### 5.9 §12 交付内容与完成标准

> **B 阶段历史快照**；下表的「不适用」是 2026-11-08 当时判定。发布 / 升级 / 实跑之后的重新判定见 §5.0A A.6。

| §12.1 交付项 | 状态 | 落点 |
|---|---|---|
| 1 代码差异及对应问题，只含 P0/P1/P2 与必要文档 | 已交付 | 31 个已改 + 3 个新文件；§5.2 / §5.3 / §5.4 逐条对应；无无关重构 |
| 2 最小验证及结果，区分本轮实测 / 旧证据 / 静态 / 未执行 | 已交付 | 本节全表 + §5.10 实测清单 |
| 3 若已发布：commit / CI / 平台 / schema / SHA-256 对照 | **不适用** | 未发布（`B1`）；本轮无新候选，§2 三行留待回填 |
| 4 若已升级：实例路径 / 备份位置 / 升级前后对照 / 重绑定核查 / 未处理对象 | **不适用** | 未升级（`B3`）；真实实例状态见顶部「当前实例状态」 |
| 5 若已实跑：退出码 / run ID / 动作与跳过原因 / 请求与计费核查 / 观察轮次 | **不适用** | 未实跑（`B5` / `B6`） |
| 6 更新一份现有验收记录；未获授权部分列为待执行 | 已交付 | 本文件；未新增第二份报告 |

**§12.2 分层状态**：六层已在顶部「当前状态」表逐层填写 —— 只有「代码修复完成 = 是」；「新版本已安装 / 身份恢复完成 / 手动清理可用 / 有限范围自动运行通过 = 否」；「待批准 / 阻塞 = C / D / E」。未用「所有测试通过」「程序退出 0」「定时任务已加载」替代后四层。

### 5.10 本轮实测清单（命令 → 结果）

```text
go build ./...                                                     → OK（编译核对，非发布门禁/原生制品构建）
go test ./cmd/dustkeeper -run 'TestScopeRebind' -count=1 -v        → 4 个用例 + 4 个子用例全 PASS
go test ./internal/store -run 'TestProveUpgradedSchema...|TestExplicitUpgrade' -count=1 -v → 全 PASS
go test ./internal/scope ./internal/store ./internal/execute \
       ./internal/review ./internal/report ./cmd/dustkeeper \
       ./migrations -count=1                                      → 6 个包 ok；migrations 无测试文件
python3 -m unittest scripts.test_run_readable_log -v              → Ran 2 tests，OK
gofmt -l <本轮改动的全部 .go 文件>                                → 无输出
git diff -- migrations/005.sql / scripts/collect-run-data.py     → 均为空（未改动）
```

阶段 A 的只读核对（`git status` / `version --json` / `status --json` / `launchctl print` / 数据库计数 /
`owner.json` / `vault-bind.json` / 6 份 manifest）为**只读实测**，结果写入 §4 与本文件顶部；未倾倒完整日志、
配置或凭据。交叉验证收尾时又以 `mode=ro` 复核一次：`events=12` / `grants=1` / `items=10` /
`observations=1384` / `plans=1` / `rules=2` / `scopes=1` / `scan_queue=218`、`PRAGMA user_version=3`、
6 个 quarantined 对象 ID 与顶部一致，且 schema-3 库确实没有 `review_dispatch` / `review_unknown` 表
（不为查询而提前升级）。**B 阶段当时未运行**：全量 `go test ./...`、`-race`、双平台原生 build、真实模型调用、
真实 `run`、真实 `scope rebind --confirm`、真实 ZIP 产物字段核对。C 阶段已补跑正式门禁与双平台原生制品，
D 阶段已执行真实 `scope rebind --confirm`，E1 已执行真实 `run` 并核对诊断 ZIP；
**真实模型调用至今仍为 0 次**（配置与凭据缺失）。

### 5.11 未执行 / 未覆盖项目与下一步

> **B 阶段历史快照**；下列 1–6 项是 2026-11-08 的缺口清单，`B1`–`B6` 的变化与剩余缺口见 §5.0A A.1 / A.7。

1. **真实实例 6 个隔离对象恢复链未取证**（§5.3 末段要求明确列出）：缺失的是「在原路径、原实例、
   真实 `dev-16777231` 上做过只读预览并逐项核对 6 份 manifest / owner / anchor / payload」的证据，以及
   「重绑定确认后按既有授权流程恢复一个对象」的真实证据。本轮只有独立临时夹具的等价证据。下一步按
   `B2 → B3 → B4` 顺序取得授权后，先执行 `scope rebind --id downloads --json` 只读预览并与备份比对，
   再决定是否确认；不以清空隔离区或新建实例绕过。6 个对象仍在隔离区，见顶部「当前实例状态」。
2. **C 阶段**：新代码提交 / 推送 / 新 CI 候选制品与 commit-schema-SHA-256 对照（`B1`）。
3. **D 阶段**：停调度、一致备份、替换二进制、`3 → 6` 显式升级、真实重绑定与前后状态对照（`B2`–`B4`）。
4. **E 阶段**：小范围真实 `run --json`、解除暂停、恢复调度、自然日历触发 1 次 + 后续 3 次（`B5` / `B6`）。
5. **长期未覆盖**：完整 30 天观察期、真实永久清空分支、全部故障恢复情形；跨启动 `dev-*` 行为的真实设备
   长期验证（`operations.md` §3.2 已注明不得写成「已彻底解决」）。
6. **发布门禁**：全量 Go/race、双平台原生制品、manifest/checksum 核对、额外独立审查均未执行；进入正式发布
   阶段时按项目现有门禁执行，若用户要求跳过须如实标注，不能称门禁通过。

**结论（B 阶段历史快照，2026-11-08）**：执行文档 §4–§9 与 §12 的可独立开展项已逐条核对完毕，无遗漏；
§10–§11 全部 29 项属**当时**未获授权的 C/D/E 阶段，已逐条列出阻塞编号与下一步。该时点只能写
「**代码修复完成**」，不可写「全部完成」。C/D/E 执行后的最新结论见顶部「当前状态」、§2 与 §5.0A。

