# 验收记录

## 当前状态（分层填写）

> 本文件是**唯一继续维护的验收入口**；下面的 §1–§4 保留历史核对与最终运行事实的填写模板，
> §5 是 2026-11-08 对执行文档 §4–§12 的逐条交叉验证映射（不是第二份结论）。
> 执行依据：2026-11-08 的《Dustkeeper：修复、升级与运行验收执行文档》（本机交接文档，仓库外，不随制品分发）。
>
> **本节在 C/D/E 阶段执行后会就地回填**；带「截至整改完成」字样的段落是 2026-11-08 B 阶段完成时点的
> 历史快照，不代表发布 / 安装 / 运行之后的状态。以下表为最新结论，回填记录见 §2 与 §5。

| 可报告的状态 | 本轮结论 | 事实 / 缺口 |
|---|---|---|
| 代码修复完成 | **是** | P0 `scope rebind`（含跨启动 `dev-*` 根因修复与 vault 旧标签证明）、P1 `review dispatch resolve` + `006.sql` / schema 6、P2 正式路径 `request_timings` 已完成；README / `operations.md` / 本文件已同步。受影响包定向测试、独立临时夹具五项验收、Python 可读日志用例通过。 |
| 新版本已安装 | **否** | 未停止调度、未做一致备份、未替换二进制、未执行 `init` 迁移；本机托管实例仍为 `2cbf8cb` / schema 3。 |
| 身份恢复完成 | **否** | 真实 `downloads` 仍 `held`；6 个隔离对象及其 manifest 未做重绑定，恢复链在真实实例上未取证。 |
| 手动清理可用 | **否** | 未获准真实运行，无真实候选 / 退出码 / run ID / 计费核查记录。 |
| 有限范围自动运行通过 | **否** | 调度未恢复，自然日历触发 1 次 + 后续 3 次观察均未执行。 |
| 待批准 / 阻塞 | **C / D / E 阶段** | 缺：本轮新代码的提交/推送与原生 CI 制品；停止 `com.dustkeeper.managed` 调度与一致备份；安装并显式 `3 → 6` 升级；对原路径提交重绑定确认；小范围真实 `run`；恢复调度并观察自然触发。旧「推送 main」批准不覆盖本轮新提交，历史服务器清理批准不覆盖本机 Downloads。 |

**本轮没有新候选制品**：代码仍在工作树中（未提交、未推送、未触发新 CI）。旧 CI run `42379326052` 与
`5be1e90` 的 SHA-256 只证明旧代码，不为本轮 P0/P1/P2 背书。正式发布时需要新候选的 commit、平台、
schema 6、二进制 SHA-256 与同次 manifest/checksum 一一对应，并把结果回填到本文件 §2 的「源码 / CI / 制品」
三行；不要把本机临时构建冒充 CI 发布制品。

**仍未处理的对象**：真实实例 `downloads` 的 6 个 quarantined 对象、4 个 planned、1 个 active grant、
2 条 rule、**1384** 条 observations、`scope_mount_binding:downloads`（旧 device `16777229` / 旧 boot session
`C0FAFF7C-…`）。`owner.json` / `vault-bind.json` / 6 份 manifest 均固化旧 `dev-*` 标签，保持原样未改。
6 个对象 ID：`obj_0ad8611b736903e35cf5d9f412bc8086`、`obj_45a5f266101e1503c0c5f3aeffec6bd1`、
`obj_55e90a06b2e406fe156819ced5c6799f`、`obj_73e549133d7b51651aa36492a6c4ae89`、
`obj_88dc6a0e88751df650a08ee6bd080e64`、`obj_b2dd2f226e3faadfd834d714dd321ce7`。
本机库仍是 schema 3，尚无 `review_dispatch` / `review_unknown` 表，不为查询而提前升级。
设备编号从 `16777229` 变为 `16777231` 且 boot session UUID 改变，但根 inode `278384`、birth time、
owner `501`、vault nonce 与 anchor tag 一致；具体成因**尚未证实**，不写成"换盘"也不写成"只是重启可忽略"。

**完成口径**：本轮只可写「代码修复完成」，不可写「全部完成」；不得用「所有测试通过」「程序退出 0」
「定时任务已加载」替代上表后四层。

---

对应 [后续执行计划](dustkeeper-next-steps-20261106.md) §10–§11。**本文件仍不是验收结论**：
源码推送、双平台 CI 与制品事实已填写，其余字段保留"待执行 / 未观察到"，不得留空后默认为通过，
也不得用 CI 结果替未部署的真实运行背书。CI 里的隔离/恢复与混合批次冒烟用的是假模型，不算真实模型调用。

**基线说明（重要）**：接手文档写的是 `abd6cfc`（`fix/closeout-review-observability`），那三次提交在本工作区
与远端都不存在。用户后续明确授权「以 8e46da5 重做」「推送 main」，因此本轮的实际基线是
`8e46da5` → `9aa6c4c` 主线，**不再是 `abd6cfc`**；文档 §2.2 的八条能力已在这条基线上重建，schema 已到 5。
差异与逐条实现见 [接手核对与重建记录](handover-20261107.md) B 段。验收记录中的"是否相对 `abd6cfc` 又有修复"
一栏不再适用，改为记录本轮主线提交。

---

## 1. 收口检查表（§10.1）

**第 1、2 项已完成并有证据；第 3–8 项仍未勾选。** 勾选制品门禁不等于"已安装"或"已真实运行"。

- [x] 本轮主线提交已推送到 `main`、通过双平台原生 CI（用户改选"推送 main"，不另建 PR）。
      证据：`8e46da5..5be1e90`；CI run `42379326052`，`linux-amd64` / `darwin-arm64` 两 job 均 success。
- [x] 最终 main 的制品、commit、SHA-256、version 有完整对应关系。
      证据：两份 CI manifest 均 `commit=5be1e90` / `version=1.0.0-phase1`，SHA-256 与 `SHA256SUMS`
      及实测一致；darwin 制品 `version --json` 得 `schema=5`。**制品未安装到任何主机。**
- [ ] 一个明确范围的目标实例完成备份、显式升级和状态核对。
- [ ] 一次小范围真实运行有有效候选、结果可解释、现场与记录一致。
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
| 源码 | **制品对应的源码提交** `5be1e90b99847aff85348d3422358d18fdc9e0c4`；其后的 `main` 若还有提交，均为纯文档回填、不重新产出制品，部署时须重新核对；PR：不适用（用户改选推送 main）；相对 `abd6cfc` 的比较：不适用（基线已改） |
| CI | run `42379326052`（push main）；`test (ubuntu-latest, linux-amd64)` / `test (macos-latest, darwin-arm64)` 均 success；build/vet/test/race/release/制品校验/归档全绿；`test/eval` 无真实端点时的 skip 仍属"未运行"，不解释为模型准确率验证 |
| 制品 | `darwin-arm64`：artifact `dustkeeper-1.0.0-phase1-darwin-arm64`，manifest `commit=5be1e90`，SHA-256 `5a7312e756142e25b088ab7792ef4856a8b4ea74d90ad273cf6be8235ffb55f7`，本机 `version --json` → `1.0.0-phase1 / schema 5 / commit 5be1e90 / SQLite 3.53.4`；`linux-amd64`：artifact `dustkeeper-1.0.0-phase1-linux-amd64`，SHA-256 `0e38e493da589595c81c9b8a96781ef67dd52a42e2249133914ac44998bfd4b6`（本机不可执行，只核对清单与摘要）。**两者均未安装** |
| 目标实例 | 主机、账号、安装路径、配置/状态目录、全部 scope 与本次范围：待执行 |
| 升级 | 旧 schema / 新 schema / 备份停机点 / 实例与历史状态核对：待执行 |
| 配置 | 实际阈值、任务/批次/请求预算、代理与通知选择：待执行（凭据值不写进文档） |
| 手动首跑 | run ID、原始退出码、有效候选与实际动作、结果页位置：待执行 |
| 调度 | 单元名、真实触发时间、退出状态、对应 run ID：待执行（手动执行不计入） |
| 后续观察 | 后续 3 次预定触发各自的结果与问题：待执行 |
| 数据 | ZIP 文件名、采集缺项、实际结果目录、覆盖时间窗口：待执行 |
| 未验证 | 没有触达的真实分支、长期指标、待授权运维事项：待执行登记 |
| 最终结论 | 源码可发布 / 手动可运行 / 有限范围自动运行：待执行，不得提前选择 |

建议结论格式（**尚未填写**）：

> 本次使用提交 `<main SHA>`、制品 `<target / sha256>`，在 `<主机 / 账号 / 范围>` 完成显式升级和一次真实运行。
> 调度已于 `<时间>` 实际触发，后续 `<3 次记录>` 已完成。实际动作与审核、请求和状态记录一致。
> 未验证范围为 `<如实列出>`。当前 `<无未处理阻断 / 列出仍阻断项>`，结论为 `<转维护 / 停留在哪一步>`。

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

### 5.0 状态口径与共享阻塞编号

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

**§5.3 末段**：真实实例 6 个隔离对象的恢复链**未能取证**，缺失证据与下一步见 §5.11；未清空隔离区、未新建实例绕过。

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
| 4 `acceptance.md` 顶部标当前状态；复用它记录新候选与最终运行事实 | 静态 | 本文件顶部「当前状态」表 + 本节；§2 的三行留待新候选回填；未新增竞争报告 |
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

| §12.1 交付项 | 状态 | 落点 |
|---|---|---|
| 1 代码差异及对应问题，只含 P0/P1/P2 与必要文档 | 已交付 | 31 个已改 + 3 个新文件；§5.2 / §5.3 / §5.4 逐条对应；无无关重构 |
| 2 最小验证及结果，区分本轮实测 / 旧证据 / 静态 / 未执行 | 已交付 | 本节全表 + §5.10 实测清单 |
| 3 若已发布：commit / CI / 平台 / schema / SHA-256 对照 | **不适用** | 未发布（`B1`）；本轮无新候选，§2 三行留待回填 |
| 4 若已升级：实例路径 / 备份位置 / 升级前后对照 / 重绑定核查 / 未处理对象 | **不适用** | 未升级（`B3`）；真实实例状态见顶部「仍未处理的对象」 |
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
（不为查询而提前升级）。**未运行**：全量 `go test ./...`、`-race`、双平台原生 build、真实模型调用、真实 `run`、
真实 `scope rebind --confirm`、真实 ZIP 产物字段核对。

### 5.11 未执行 / 未覆盖项目与下一步

1. **真实实例 6 个隔离对象恢复链未取证**（§5.3 末段要求明确列出）：缺失的是「在原路径、原实例、
   真实 `dev-16777231` 上做过只读预览并逐项核对 6 份 manifest / owner / anchor / payload」的证据，以及
   「重绑定确认后按既有授权流程恢复一个对象」的真实证据。本轮只有独立临时夹具的等价证据。下一步按
   `B2 → B3 → B4` 顺序取得授权后，先执行 `scope rebind --id downloads --json` 只读预览并与备份比对，
   再决定是否确认；不以清空隔离区或新建实例绕过。6 个对象 ID 见顶部「仍未处理的对象」段。
2. **C 阶段**：新代码提交 / 推送 / 新 CI 候选制品与 commit-schema-SHA-256 对照（`B1`）。
3. **D 阶段**：停调度、一致备份、替换二进制、`3 → 6` 显式升级、真实重绑定与前后状态对照（`B2`–`B4`）。
4. **E 阶段**：小范围真实 `run --json`、解除暂停、恢复调度、自然日历触发 1 次 + 后续 3 次（`B5` / `B6`）。
5. **长期未覆盖**：完整 30 天观察期、真实永久清空分支、全部故障恢复情形；跨启动 `dev-*` 行为的真实设备
   长期验证（`operations.md` §3.2 已注明不得写成「已彻底解决」）。
6. **发布门禁**：全量 Go/race、双平台原生制品、manifest/checksum 核对、额外独立审查均未执行；进入正式发布
   阶段时按项目现有门禁执行，若用户要求跳过须如实标注，不能称门禁通过。

**结论**：执行文档 §4–§9 与 §12 的可独立开展项已逐条核对完毕，无遗漏；§10–§11 全部 29 项属未获授权的
C/D/E 阶段，已逐条列出阻塞编号与下一步。本轮只能写「**代码修复完成**」，不可写「全部完成」。

