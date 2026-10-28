# Dustkeeper 合规与验收状态

> 权威部分：下面的 **FINAL 状态表（A01–A12）**，对应 `dustkeeper-final-simplification-plan-20261027.md`
> （下称"最终计划"）。本文件从 §4 起是第三轮（2026-10-26）与第二轮（2026-10-27）核对的**原文**，
> 保留作为过程记录，**不再驱动新需求**；与 FINAL 状态表冲突处以状态表为准。
>
> 判定：**通过** = 有能复现旧问题的用例/断言，且本轮实跑输出对得上；
> **部分** = 主路径有断言，但计划描述里有未覆盖的分支；
> **未验收** = 没有证据，明确不宣称。

## 1. FINAL 状态表

本轮五个提交：F1 `50c9df3`（去重/adopt 退出）、F2 `1c7b1ec`（启用原子化）、F3 `754a21d`（执行闭环）、
F4 `0f9a371`（扫描/日志/盘点如实）、F5 `82c29ee`（两个原生目标发布 + 文档收口）。

FINAL-1.0 之后按独立复核报告修补了 D01–D14（基于 `9d9af21`），逐条修改、反例与结果见 **§1.1**。
下表各行的证据已把 §1.1 的提交并入，判定按并入后的实际覆盖重新给出。

| ID | 验收组 | 判定 | 证据（commit / 命令 / 结果） |
|---|---|---|---|
| A01 | 有限旧格式兼容 | 通过 | F1 `50c9df3`：`go test ./test/integration -run TestSlimLegacyCompatibility -count=1`。三个子用例：hot journal 先由 SQLite 回滚再核版本；当前格式正常打开；其它版本写出的库在写入前以 `INCOMPATIBLE_SCHEMA` 拒绝，且文件字节的 sha256 在拒绝前后相同。旧 vault 不被改写、正常 lifecycle pending 不被整体拒绝、terminal 事件不重复写终结由同一用例与 `TestCompleteQuarantineIsIdempotent` 覆盖。D11 `7ac620a`：首次初始化改为在 `state.db.init` 上建库、逐条迁移、全部提交后才 `os.Link` 成 `state.db`——`TestAnInterruptedFirstInstallationResumes`（停在 000/001/002 三个迁移点）、`TestAnEmptyStateFileIsInitialisedNotRefused`、`TestARealOlderStateFileIsStillRefused`（旧格式库字节不变且不留 staging）。 |
| A02 | 去重/adopt 真正退出 | 通过 | F1 + F3：`TestSlimLegacyActionsAreBlocked`（`reference` scope 注册被拒且不建 vault；duplicate 计划经 `plan apply` 被拒）、`TestSlimUnresolvedObjectBlocksBothEntrances`（普通入口与高级入口都拒，且不产生新 intent）、`TestAQuarantineOnlyApprovalNeverBecomesADeletion`。健康旧对象仍可恢复、且恢复后 keeper pin 释放：`test/integration/slim_f1_test.go:351-356`；不支持 pending 未被清空：同一用例逐行核对 pending 行数。CLI 侧 `dedup`/`adopt` 命令已不存在（`cmd/dustkeeper/main.go` 路由表）。 |
| A03 | 范围和规则严格性 | 通过 | `TestSlimLegacyActionsAreBlocked` 的 reference 子用例（非 `managed_disposable` 不自动 Q/P）；`internal/rules` 的字段合同用例：`TestEveryDocumentedFieldIsRequired`、`TestRequiredFieldsRejectNull`、`TestEmptyCollectionsAreNotMissingFields`、`TestParseRejectsTwoSpellingsOfOneField`、`TestParseDraftRejectsCaseVariantsOfItsOwnKeys`、`TestModelDraftsGoThroughTheSameFieldContract`；歧义/不支持语义不生成可执行草稿：`internal/rules/corpus_test.go` + `test/eval` 的 semantic 判定（模型侧结论见 A12）。 |
| A04 | 原子、幂等启用 | 通过 | F2 `1c7b1ec`：`go test ./internal/store -run TestSlimEnableTransaction`、`go test ./cmd/dustkeeper -count=1`（`cmd_rule_enable_test.go`）。覆盖：无 digest 不改 rules/grants；陈旧 digest 拒绝；中途错误整体回滚（rules 与 grants 都不留痕）；相同 active binding 重复启用不新增 grant；撤销后重新启用不复活旧 ID；Q/P 上限分开取。D01/D09 `d0f9dfb`：`replaceGrantTx` 先撤销同能力其余 active 再复用同 digest 的行，`ActiveGrant` 读全量并拒绝多活动授权；确认命令带这次屏幕上的实际额度（`TestSlimSmallerApprovalWins`、`TestSlimEnableCommandCarriesTheApprovedCeilings`）。 |
| A05 | 权限和保留期不追溯扩大 | 通过 | `TestAutoPurgeIsNotRetroactive`（先隔离后开自动清空，旧对象仍不可清空）、`TestZeroBudgetForbidsTheAction`、`TestAutomaticPurgeHonoursTheGrantCeiling`、`TestAQuarantineOnlyApprovalNeverBecomesADeletion`（Q 授权不会被当成 P 授权）。 |
| A06 | 正常 Q/R/P 真能完成 | 通过 | 源码：`TestQuarantineMovesOnlyWhatTheRuleSelected`（命中隔离，排除/受保护/不命中保留）、`TestRestoreRefusesToOverwrite`、`TestManualPurgeRemovesOnlyListedObjects`、`TestSlimClockSeamDrivesRetention`（受控时钟推进后授权 P 成功）；仅 Q 模式到期仍保留由 `TestAQuarantineOnlyApprovalNeverBecomesADeletion` 覆盖。制品：`darwin-arm64` 的 Q/R 冒烟，见 §3。 |
| A07 | 动作前证据与资源检查 | 通过 | `TestDiskPressurePausesQuarantineButNotADuePurge`（低空间零新 Q，到期清空照常）、`TestBudgetExhaustionLeavesTheRestAlone`、`TestClockGoingBackwardsSuspendsPurge`、`TestAcknowledgingTheClockRestartsUnprovableWindows`、`TestANonZeroInodeFlagDoesNotBlockThePurge`（反向：正常 rename 正例通过）。 |
| A08 | 保护及控制不绕过 | 通过 | `TestPurgeFreezesWhenAliasCannotBeResolved`、`TestAPlanDoesNotOfferAnObjectWhoseAncestryCannotBeInspected`（预览与执行同一套祖先判定）、`TestSlimUninspectableProtectionRefusesThePurge`、`TestSlimControlPropagation`（`stop_run` 即使 Go error=nil 也非成功退出，后续零新 intent）、`TestPauseStopsNewIntents`、`TestGlobalProtectionWinsOverTheRule`。D04 `27c65e4`：对账无法记录结果时本轮零 scope 启动并 `ControlStopRun`，时钟仍记录（`TestARunThatCannotRecordItsOutcomesStartsNothing`）。 |
| A09 | 故障可收敛 | 部分 | 已有：`TestReconcileRetriesEveryActionUntilTheScopeIsFree`（临时故障连续两轮不终结，解除后完成）、`TestReconcileCompletesACrashedQuarantine`（中断恢复）、`TestCrashDiscoveredMiscaptureRecordsTheActualObject`、`TestWrongCaptureIsPutBackWithProtections`、`TestCompensationCrashReturnsToTheHold`、`TestSlimTwoPurgeRoutesAgree`（同一初始状态下 `purge apply` 与 `plan apply` 逐字段相同）。D02/D03/D06/D07/D08 `27c65e4` 补了"证据成立才收敛"的一半：验证窗口内被改动的源被拒（`CodePlanStale`，不写成 Held）、不可读的 manifest/vault 对象 park 而不是当结论、stat 失败的源不算证据、合同坏/绑错/无 prior 状态一律不猜、补偿成功收敛为 `StateRestored` + scope 需重新确认、目录同步失败的 manifest 被撤回。**未覆盖**：Q/R/P 各自"系统调用成功后注入同步/complete 失败"的完整矩阵——`internal/testkit` 故障注入点仍不存在，现有用例只在现有协议点上手工插入中断或者用一次性测试钩子。按最终计划 §6.4-1 的口径，这一组只能算部分。 |
| A10 | 扫描完整性诚实 | 通过 | F4 `0f9a371`：`TestSlimIncompleteScanKeepsTheEpochOpen`（去掉 `unverifiedCount > 0` 分支后报 "an entry that could not be inspected completed the epoch"）、`TestSlimVanishedEntryIsNotAFailure`（ENOENT 只记 vanished，目录照常完成）、`TestSlimIncompleteScopeStartsNoAutomaticQuarantine`（去掉 `!scanState.Complete` 分支后报 "collected 2 objects"）、`TestAFailedDirectoryCannotCompleteTheEpochOrRetireItsOwnObservations`。 |
| A11 | 诊断不修改他人文件、不虚报缺失 | 通过 | F4 `0f9a371`：`internal/diag` 六个拒绝用例（symlink / FIFO / 宽权限 / 硬链 / 目录 / 设备，全部 `ErrNotPrivate` 且原对象字节与 mode 不变）+ `TestAPrivateLogIsCreatedAndStillRotates`；`internal/rescue`：`TestSlimRescueEntryLevelUnverified`、`TestSlimRescueDirectoryLevelUnverified`（回退成 `read` 即报 `object_state="read", want "partial"`）、`TestSlimRescuePartialManifestsDoNotBecomeOrphans`、`TestAnUnreadableObjectsDirectoryDoesNotClaimTheObjectsAreGone`。D12 `e14855c`：日志目录持 FD 并校验（拒 symlink/非目录/group-other 可写/非本人），创建与轮转都相对该 FD（`TestASymlinkedLogDirectoryIsRefusedAndItsTargetUnchanged`、`TestARotatedNameStaysInsideTheVerifiedDirectory`）。D13 `e14855c`：`readManifests` 只有 ENOENT 才算 missing，orphan 判定排除 partial 与 unreadable；零计数 + 目录不可读也 `RECOVERY_REQUIRED`（`TestRecoverInspectDoesNotCallObjectsOrphansWhenManifestsAreUnreadable`、`TestRecoverInspectWithNoEntriesAndAnUnreadableDirectoryStillFails`）。 |
| A12 | 有限发布证据 | 部分 | 两个目标都有本轮 checksum 与真实执行记录（§3）：`darwin-arm64` 在本机产出并冒烟；`linux-amd64` 在 CI 的原生 x86_64 runner 上产出并冒烟（`gh run 40642359982`，两 job 均 success）。两个平台的 `build`/`vet`/`test`/`race` 分别在本机与 CI 跑过，CI 每个 job 构建前先断言 runner 实际架构等于声明目标。10 万文件扫描完成性成立：wall 54.5 s、峰值 RSS 29.5 MiB ≤ 256 MiB（§2，`commit` 记在证据文件里）。D14 `1ffbc43` 把发布门禁收紧到"证据与产物必须对得上"：libc 判定改读真实链接输入、Q/R 冒烟独立断言移动与内容、后段失败清理本轮产物、CI 归档本目标四个文件、宿主断言改用 `GOHOSTOS/GOHOSTARCH`。**仍差一项**：真实模型的固定语料报告——本机没有获准端点，`test/eval` 保持 skip，不以 skip 当通过；另外 CI 归档步骤本轮**未实跑**（见 §1.1 的 D14 行）。因此这一组记部分。 |

结论：12 组里 **10 组通过、2 组部分（A09、A12）、0 组未验收**；A09 的部分是 Q/R/P 同步/complete
失败矩阵未建（`internal/testkit` 注入点不存在），A12 的部分是真实模型语料报告缺失、以及本轮新增的
CI 归档步骤尚未实跑。两条都写在这里，不用"结构齐全"顶替；也没有把未执行的检查算成通过。

### 1.1 复核报告 D01–D14 的修补（2026-10-29，基于 `9d9af21`）

范围纪律：只修报告点名的缺陷；不恢复 dedup/adopt，不加新服务，不改磁盘合同版本，不引入通用框架。
每条都先在旧代码上跑出失败（保留钩子、只删修复块，或整文件回退到 `9d9af21`），再让同一条断言在新代码上通过；
"全量测试通过"不作为任何一条的证据。

| 缺陷 | 修改 | 反例（旧代码上的实际结果） | 结果 | 未关闭 |
|---|---|---|---|---|
| D01 授权可并存 | `d0f9dfb`：`replaceGrantTx` 先撤销同能力其余 active 行，再复用同 digest 的行或插入；`ActiveGrant` 读全量并拒绝多活动授权 | 先确认 100 再确认 10 → `TestSlimSmallerApprovalWins` 报 "2 active purge grants after confirming 10 files" | 通过：同一确认只剩一条 active，相同 digest 复用 id | — |
| D02 验收不比对身份 | `27c65e4`：稳定读后与 accept 前各比对一次（`sameStableObject`、`ConfirmUnchanged`），失败走 `abandonPreparation` 并返回 `CodePlanStale` | 删掉稳定读比对 → "a file touched inside the verification window was accepted"；删掉 `ConfirmUnchanged` → 接受前被改动的源仍被隔离 | 通过：文件没动就不写 Held，退回 stale 可重试 | — |
| D03 读不到当成结论 | `27c65e4`：`readManifest` 三分类（ENOENT / 坏 JSON → 需对账，读错误 → park）；`statEntry`/`statVault` 返回 `(id, found, error)`；`captureEvidence` 返回分类错误 | 回退 `reconcile.go`：坏 manifest 记成结论（Held:1）、不可读 vault 对象被当结论、stat 失败的源被当成证据（Completed:1） | 通过：三条都不再收敛出终结状态 | Q/R/P 同步/complete 失败矩阵仍未建（见 A09） |
| D04 记录失败仍继续 | `27c65e4`：`RecordFailures>0` → `ControlStopRun` + `CodeStateWriteFailed`，本轮零 scope 启动，但 `guard.RecordRun` 照常执行 | 回退 runner：该 scope 已 `Scanned:1`，即"记账没写成却照样动文件" | 通过：`TestARunThatCannotRecordItsOutcomesStartsNothing`，退出码非零 | — |
| D06 补偿后锁死 | `27c65e4`：补偿成功 → `StateRestored` + `AbortIntent(..., StateRestored)` + `SetScopeStatus(ScopeNeedsReapproval)` | 删掉这三行：补偿过的对象停在 held，scope 不再工作（`a compensated object settled as held, not restored`） | 通过：`TestACompensatedMiscaptureLetsTheScopeWorkAgain` 走完整 Q→R | — |
| D07 合同不绑定 | `27c65e4`：`operationContract` + `checkContractBinding`（item/scope/object/base64 路径四项），`priorStateOf` 只认已记录状态 | 回退 `reconcile.go`：坏合同记 Completed:1、绑到别的 item 被 Aborted:1、无 prior 猜成 quarantined、restore 退回 quarantined | 通过：四条各自有断言 | — |
| D08 manifest 撤回 | `27c65e4`：`durableManifestsDir` 失败即 `RemoveManifest` 并如实说明 | 回退 `vault.go`：目录同步失败的 manifest 被接受；只留注入不撤回 → "the installed manifest was left behind"，同一 plan 之后被堵 | 通过：撤回后可重试 | — |
| D09 确认命令与展示脱节 | `d0f9dfb`：`next_steps`/`PLAN_STALE` 都带这次的实际额度；`ceilings` 变成带 tag 的对象；人类输出补 scope 根路径、规则解释、有界样本 | 确认命令丢掉 `--max-files` → 照抄即 `PLAN_STALE`（`the confirmation command dropped "--max-files 5"`） | 通过 | — |
| D10 FIFO 阻塞 | `7ac620a`：`openStateFile` 加 `O_NONBLOCK`（open(2) 在 fstat 之前就会等写者） | FIFO 当状态库：旧代码 3 秒超时未返回 | 通过：`TestAFIFOIsRefusedWithoutBlocking`，FIFO 未被写 | — |
| D11 首次初始化不留半成品 | `7ac620a`：在 `state.db.init` 上建库并逐条迁移，全部提交后 `os.Link` 发布、`unlink` 暂存名、`fsync` 目录；已发布的旧格式库仍拒绝 | 中断的首次初始化留下 staging 名，且旧代码会把它当既有库 | 通过：三个中断点都能续跑；旧格式库字节不变、无残留 | — |
| D12 日志目录边界 | `e14855c`：`openLogDir` 用 `O_DIRECTORY\|O_NOFOLLOW` 打开并校验属主/权限，持有 FD；创建用 `openat`，轮转用相对该 FD 的 `unlinkat`/`renameat` | 软链 logs 目录被接受并写到目标；把 logs 改名后轮转落到新目录 | 通过：目标文件字节与 0600 不变；轮转名留在已验证目录 | — |
| D13 盘点把"读不了"当"没有" | `e14855c`：`readManifests` 只有 ENOENT 是 missing，其余 unreadable；orphan 判定排除 partial/unreadable；CLI 零计数也 `RECOVERY_REQUIRED` | manifests 目录 chmod 000 → `manifest_state=missing`、`orphan_objects=1`；空 manifests + objects 不可读（四项计数全 0）→ **exit 0** | 通过：unreadable、orphan=0、零计数也 exit 6 | — |
| D14.1 libc 判定依赖目录名 | `1ffbc43`：探针改成真实静态链接（失败即失败），证据取自 driver 转录里 collect2 的 startfile 与 `-L`，两者必须同为 musl；libc.a 不再作证据 | 空 `musl-headers-placeholder` CPATH 目录 + 真实 glibc gcc → "an empty musl-named include directory proved a glibc compiler" | 通过：容器内真实 musl-gcc 仍通过（`startfile=/usr/lib/x86_64-linux-musl/Scrt1.o`、`musl_libdir=-L/usr/lib/x86_64-linux-musl`、`musl_pkgs=musl=1.2.3-1,…`） | 通用入口没有第三种 libc 目标，也不新增 |
| D14.2 Q/R 冒烟可 no-op | `1ffbc43`：保存原始字节；apply 后断言原路径不存在；restore 后 `cmp` 比内容；再列一次确认对象已不在可恢复列表 | 完全不移动文件 → 旧脚本 exit 0；restore 写错内容 → 旧脚本 exit 0 | 通过：两条都失败在具体断言上 | — |
| D14.3 失败留半成品 | `1ffbc43`：`fail_release` 清理本轮 binary/SHA256SUMS/manifest；启动先删上一次 manifest；清单临时名写完再 `mv` | shasum 写失败、SHA256SUMS 校验失败 → 本轮 binary 与上一份 manifest 都留在 dist | 通过：其他版本产物不被删 | — |
| D14.4 CI 未归档 | `1ffbc43`：验证后 `upload-artifact` 归档本目标 binary + SHA256SUMS + release-manifest + build-facts，`if-no-files-found: error` | 本次 API 返回 `artifacts=[]`（报告所记） | 工件已加进 job | **未实跑**：本轮没有 CI 运行，归档是否真出现在 run 里没有证据 |
| D14.5 宿主断言用目标变量 | `1ffbc43`：脚本与 CI 改用 `GOHOSTOS/GOHOSTARCH`，并拒绝与 `GOOS/GOARCH` 不一致 | `GOOS=linux` 在 darwin 宿主上跑 → 旧脚本按宿主名产出，失败信息里也没有宿主/目标之分 | 通过：`TestACrossCompiledSettingIsRefused` | GOHOST 不是虚拟化/硬件鉴定，本期不做 |

同时按 §6.3/§6.4 收了三处口径：`ru.Inblock` 存原值并改名 `inblock_ops`（不再 ×512 冒充字节）、
`defaultBudgetRun` 复用续扫的 OK/PARTIAL 检查（真实故障结果不能生成性能证据）、README 主流程改成
`rule enable`（预览 + 确认两次调用，示例已按顺序实跑）、本文件第 31–32 行的"9 组通过 + 2 组部分"改为
与表一致的 10 + 2。

## 2. 本轮实跑（2026-10-29，darwin/arm64, APFS，`commit=1ffbc43`）

```sh
go build ./...                 干净
go vet ./...                   干净
go test ./... -count=1         exit 0，18 个包 ok
go test -race ./... -count=1   exit 0，18 个包 ok
./build/test-linux.sh          exit 0，18 个包 ok（容器内 ext2/ext3 loop，aarch64）
./build/build.sh --release     exit 0，darwin-arm64 制品 + 清单 + SHA256SUMS
```

10 万文件扫描（`DUSTKEEPER_BENCH=1 DUSTKEEPER_BENCH_FILES=100000`，证据文件里记了 `commit`）：

| 项目 | 结果 |
|---|---|
| 工作量 | 100 000 文件 / 1 051 目录，完整 epoch，队列 pending 0、failed 0，observation 100 000 行 |
| 资源 | wall 54 469 ms（CPU 1 992 + 4 461 ms），峰值 RSS 29.5 MiB（上限 256 MiB） |
| 数据库 | 180.0 KiB → 59.0 MiB，58.9 MiB 增长，619 B/文件 |
| 默认预算下一轮 | 扫描完整、未被预算中断、`control=continue`、envelope `code=OK`，wall 48 444 ms，峰值 RSS 29.6 MiB |
| 中断续跑 | 10 批，累计 1.00× 树大小，最长单批 5 257 ms，各批峰值 30.2 MiB |

口径：`inblock_ops` 是 `ru_inblock` 原值（文件系统输入**操作次数**），不再乘 512 当字节；
`content_bytes_read` 为 null（扫描不读内容）；WAL 字节标"不适用"（存储固定 `journal_mode=DELETE`）。
原始 JSON 与生成命令在 [`docs/evidence/performance-100k.md`](evidence/performance-100k.md) 同目录的 `.json`。
本轮 wall 比上一轮长（54.5 s vs 39.6 s）是同一台机器上并发跑其它构建的结果，没有为了数字好看调整资源预算。

`linux-amd64` 发布路径（`./build/build-linux-release.sh`，固定 digest 镜像）本机结果：

```
builder image: golang:1.26-bookworm@sha256:88750ccd…daf9d9
target:        linux/amd64
go:   go version go1.26.10 linux/amd64
musl: x86_64-linux-gnu-gcc (Debian 12.2.0-14+deb12u1) 12.2.0
musl packages: musl=1.2.3-1,musl-dev=1.2.3-1,musl-tools=1.2.3-1
release: linux-amd64 failed the quarantine/restore smoke (step 11); see the log
smoke: step 11 failed: dustkeeper scope add --path … --kind managed_disposable --name smoke --json
{ "code": "ACCESS_DENIED",
  "message": "fsops: exclusive rename is not supported here: openat2 unavailable (function not implemented)" }
       this runtime cannot execute the artifact: openat2 is not available to it,
       so the release has to be built on a native linux-amd64 host
release is incomplete: linux-amd64 produced no acceptable artifact
```

这是设计的结果，不是被吞掉的失败：构建、`version`、ELF 复核都通过，冒烟跑不动就整轮失败，
`dist/` 里不留下这个目标任何产物（`fail_target` 会删掉半成品；`1ffbc43` 之后连本轮清单也会一起清掉）。
这一轮的 libc 证据记在 `dist/build-facts.txt` 里，取自真实链接输入：

```
target=linux-amd64 format=elf arch=amd64 linkage=static toolchain=x86_64-linux-gnu compiler=musl-gcc
  startfile=/usr/lib/x86_64-linux-musl/Scrt1.o musl_libdir=-L/usr/lib/x86_64-linux-musl
  libc_a=/usr/lib/gcc/x86_64-linux-gnu/12/../../../x86_64-linux-gnu/libc.a
  musl_evidence=/usr/lib/x86_64-linux-musl/Scrt1.o
  musl_pkgs=musl=1.2.3-1,musl-dev=1.2.3-1,musl-tools=1.2.3-1
  artifact=[format=elf machine=amd64 linkage=static interp=none needed=0]
```

注意 `libc_a` 那一行仍是 glibc 的路径，而链接命令是 musl 的——这正是 `1ffbc43` 不再拿
`-print-file-name=libc.a` 当证据的原因。原因已定位到运行时而非制品：
同一镜像在原生 arm64 容器里 `openat2` 正常（`errno=18 EXDEV` 是路径跨挂载的预期返回），
在 amd64 容器里是 `errno=38 ENOSYS`（arm64 宿主上的 Rosetta）。

### 2.1 同一个脚本在原生 x86_64 上：CI（`gh run 40642359982`）

`ubuntu-latest`（x86_64）跑 `./build/build-linux-release.sh` 的结果，两个 job 均 success
（这是 `d823542` 时的门禁输出；`1ffbc43` 把证据换成链接输入之后没有新的 CI 运行，见 §1.1 的 D14.4/D14.5）：

```
digest: sha256:88750ccd…daf9d9   (固定 builder 镜像)
go:   go version go1.26.10 linux/amd64
musl: x86_64-linux-gnu-gcc (Debian 12.2.0-14+deb12u1) 12.2.0
dustkeeper-1.0.0-phase1-linux-amd64: machine=Advanced Micro Devices X86-64 interp=0 needed=0
linux_libc=musl-static
target=linux-amd64 format=elf arch=amd64 linkage=static toolchain=x86_64-linux-gnu
  compiler=musl-gcc musl_evidence=/usr/include/x86_64-linux-musl
  artifact=[format=elf machine=amd64 linkage=static interp=none needed=0]
target=linux-amd64 smoke=ok smoke_output="dustkeeper 1.0.0-phase1 (linux/amd64, SQLite 3.53.4, commit d823542)"
target=linux-amd64 smoke_qr=ok flow="init,scope add,scan,rule import,rule preview,rule activate,plan create,plan apply,quarantine list,quarantine restore"
sha256=1fa83ce4e35553b07d37e273e501abf4515f27855fb1ac2448e4ae066633ffd6
dustkeeper-1.0.0-phase1-linux-amd64 sha256=1fa83ce4…ffd6 (matches the manifest)
```

这套门禁第一次跑就抓到两个真问题，两个都修了而不是绕开：容器内 `go build ./build/verify-artifact`
因 VCS stamping 退出 128（挂载点属另一个 uid，git 拒绝该仓库）；以及制品 `version` 当时显示
`commit unknown`（同一个 git 拒绝让 `git rev-parse` 落空）。前者补 `-buildvcs=false`，后者只把
`/src` 加进容器内的 `safe.directory`——commit 仍由 git 给出，不由调用方注入。

## 3. 制品执行过哪些动作（与源码测试分开记录）

| 制品 | checksum | 在哪里、真实执行过什么 | 源码测试能证明的 |
|---|---|---|---|
| `dustkeeper-1.0.0-phase1-darwin-arm64` | `sha256=16d0b8397e8c3b081420221325e65971c05e0f8df722c001d73db636d3950263`（`commit=1ffbc43`） | 本机（darwin/arm64）：`version`；临时目录内 `init → scope add → scan → rule import → rule preview → rule activate → plan create → plan apply → quarantine list → quarantine restore`，并**独立**核对原路径在 apply 后不存在、restore 后字节与原件逐字节相同、该对象不再出现在可恢复列表（本轮新增断言） | 受控时钟下的到期 P、两条 purge CLI 等价、中断恢复、保护与预算拒绝、扫描完整性、日志与盘点拒绝 |
| `dustkeeper-1.0.0-phase1-linux-amd64` | `sha256=1fa83ce4e35553b07d37e273e501abf4515f27855fb1ac2448e4ae066633ffd6`（`commit=d823542`，**本轮未重跑**） | CI 的 `ubuntu-latest`（x86_64）容器内，同一串动作 + `readelf` 断言 X86-64 / `interp=0` / `needed=0`。这一行的冒烟早于 D14.2 的独立断言，只覆盖"命令序列跑通 + JSON 里 restorable"，不覆盖"apply 后源不存在 / restore 后字节一致" | 同上（同一套源码测试在 linux 容器 ext2/ext3 loop 上 18 包 ok，`1ffbc43` 实跑） |

这张表的作用是防止一种常见夸大：源码测试通过不等于"每个 checksum 都跑过 Q/R"。
两个 checksum 各自在一个能执行它的环境里跑过完整 Q/R，本机不能执行的那个不在本机宣称。

## 4. 历史记录：第三轮核对（2026-10-26）

> 以下为 `dustkeeper-phase1-implementation-20261025.md` 的逐节核对原文。其中的缺口（磁盘门槛、时钟确认、
> 日志子系统、去重内存等）已由后续两轮与 FINAL 计划分别处置：已修的见 §1 与第 5、6 节，
> 去重整体退出见 A02。**本节不再驱动新需求。**

# Dustkeeper 一期实现与方案文档的合规核对

> 核对对象：`dustkeeper-phase1-implementation-20261025.md`（下称"文档"）第 2–16 节。
> 核对方式：逐节读代码 + 在本机（darwin/arm64, APFS）实跑，不以"设计上应该有"代替证据。
> 核对日期：2026-10-26。
> 判定：**符合**（行为与文档一致）／**偏离**（实现了，但与文档写法不同）／**缺失**（文档要求的行为不存在）。

### 0. 结论先说

文档 §2.3 的 15 条不变量，13 条有实现与测试证据。**2 条此前不成立，本轮已修**（§9.5 keeper 依赖、§11.4 保护别名冻结）。

但**不能按 §16 的验收清单签字**。还有 2 项 P0、5 项 P1 行为缺口，以及 §14.1 里 10 个用例（A02/A03/A04/A09/A10/A13/A19/A20/A21/A23）没有可执行断言。其中最要紧的是：

1. **磁盘压力门槛没有接入执行器**——配置项存在、进入计划摘要，但没有任何代码在低空间时阻止新隔离（违反不变量 11 与 §4.3 的明文规定）。
2. **时钟确认会一笔勾销不确定性**——`doctor --acknowledge-clock` 之后立刻恢复清空，没有把"无法证明保留期"的对象 `purge_not_before` 延后到"确认时刻 + 完整保留期"（§11.2 明确禁止这种抹平）。

### 1. 本轮已修复（7 项）

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

### 2. 仍未满足（按严重度）

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

### 3. §14.1 验收用例覆盖矩阵

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

### 4. 核对通过、且有实测证据的部分

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

### 5. 建议的收尾顺序

1. **P0-1 磁盘门槛**、**P0-2 时钟确认**——两处都直接对应文档的强制条款，改动小、可测。
2. **A13 + A21**：写一个真正的"双授权 → 到期 → 后台清空"集成用例，顺带覆盖公平性与 50% 预算。这是产品唯一的主张能力，目前只有手工记录。
3. **A04 崩溃矩阵 + A02/A03**：需要先补 `internal/testkit/faults.go`（可注入 rename/sync/commit 失败），否则 §10.6 的失败点表格无法验证。
4. **P1-1 只读清单**、**P1-3 日志轮转**：运维可见性的两条腿。
5. **P1-4/P1-5/P1-6/P1-7**：收紧 store 防线与证据链，成本低但对审计友好。
6. **包内单测**（scan/execute/recovery/dedup/run）与 `internal/report`、`internal/testkit` 的落位。

### 6. 一句话判断

实现的主干——排他 rename、先记账后动手、不覆盖恢复、对账矩阵、预算取最小值、摘要绑定——是**按文档写的**，不是"看起来像"。差距集中在两类：**资源与时间这两条全局约束还没有落到执行器**（P0），以及**故障注入类的证据链几乎为空**（A02/A03/A04/A09/A10）。在这两项补齐之前，§16 的验收清单不能勾完。

---

### 5. 历史记录：第二轮复核（2026-10-27）

<small>以下为 `dustkeeper-code-review-and-next-plan-20261026.md` 的整改对照原文。其中 R14 去重内存、去重指标等条目已由 FINAL F1/F4 处置（去重整体退出、11 万指标删除），不再驱动新需求。</small>


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

### 7.1 逐项状态

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
| R13 | 脱离数据库的救援入口与运行诊断 | 关闭（P1 + P2，A23 覆盖） | **不可读≠不存在**：`rescue.Inspect` 现在按目录状态给结论——`object_state=read` 才能说"某个对象不在"；`missing`（整个 objects 目录 ENOENT）是对全体的确证；`unreadable` 只产出 `unverified`（原来会产出 `manifest_without_object`，把"没看成"说成"没了"）。单条目 stat 失败也从 `conflict` 改判 `unverified`。CLI 输出 `objects directory: <state>` 与 `unverified` 计数，`unverified>0` 时退出 6 并给出 `next_steps`。**诊断**：`internal/diag` 写 `<state>/logs/dustkeeper.jsonl`（2 MiB × 3 轮转、单字段 4 KiB 截断、0600、UTC RFC3339Nano），`phase=begin\|result\|run\|scope\|op` 五类记录，字段含 run_id / command / instance / scope_id / plan_id / object_id / op_id / item_id / action / outcome / code / status / exit / retryable / control / next_steps / message 与本轮读取字节、动作、待处理计数；`recovery.Result` 增加 `op_id` 并逐条入日志。用例：`TestAnUnreadableObjectsDirectoryDoesNotClaimTheObjectsAreGone`、`TestAMissingObjectsDirectoryIsReportedAsMissingNotUnverified`、`TestRecoverInspectReportsAnUnreadableObjectsDirectory`、`TestRecoverInspectOnAReadableDirectorySucceeds`、`TestTheLogRecordsTheRunAndItsConclusion`、`TestTheLogNeverCarriesTheModelKeyOrFileContents`、`TestEveryRecordUsesTheDocumentedKeys`、`TestARecordCarriesTheDiagnosticFields`、`TestTheLogRotatesAndStaysBounded`、`TestAnOversizedFieldIsTruncatedNotDropped`、`TestANilLoggerIsSafe`、`TestTheLogFileIsNotWorldReadable`。**仍缺**：执行期 Q/P intent 的 op_id 没有逐条进日志（只有对账结果有），`internal/rescue` 无包内单测 |
| R14 | 性能证据 | 部分关闭（扫描有界已证，去重内存未关） | 证据文件由运行生成，不手写：`docs/evidence/performance-100k.md`（10 万、30% 重复）与 `docs/evidence/performance-1m.md`（100 万、0% 重复），各带 JSON sidecar 与树形/大小分布/fan-out。10 万（30% 重复）：扫描 100 000 文件 / 1 051 目录、wall 38.5 s（CPU 1.7+3.4 s），epoch 完整、队列 pending 0、observation 行数 100 000、峰值 RSS 30.0 MiB、库 59.0 MiB（619 B/文件），逻辑重复字节 52.3 MiB；中断续跑 8 批累计扫描 1.00× 树大小、最长单批 5.2 s、各批峰值 31.0 MiB。100 万（0% 重复）：扫描 1 000 000 文件、wall 79.9 s（CPU 6.9+14.6 s）、峰值 RSS 30.8 MiB、库 588.5 MiB（617 B/文件）；续跑 17 批、1.00×、最长单批 7.8 s。**未关闭**：`dedup` 峰值 RSS 389 MiB（4.1 KB/文件；100 万、0% 重复时 1 158 MiB / 1.2 KB/文件）随文件数线性增长，超出 256 MiB 扫描门槛——已改为按大小分组流式处理（不再同时持有 bySize/byHash/结果三份索引），但结果集本身仍是每重复组两个候选，需分页/逐组落盘才能达标。顺带修掉一处一直错报的数字：`dedup.Result.LogicalBytes` 声明了、CLI 也在报，但从未赋值，所以每个重复计划都写 `logical_bytes: 0`——"这个计划能省多少"被回答成 0。扫描 wall 时间也被**每目录一次提交**主导（2 万文件 31.5 s、10 万文件 38.5 s，目录数同为 1 051，约 30–37 ms/目录；`synchronous=EXTRA` 下每目录 claim 与 done 各一次提交/fsync，文件数从 2 万涨到 10 万只多 7 s）。这两条按 §6.4-10 记为未关闭 |
| R15 | 发布脚本与平台声明 | 关闭（真实 musl + 产物检查 + 冒烟） | `build/build.sh` 对每个 linux 目标做**双重证明**：编译器 `-v` 输出里必须有 musl 证据（`musl-gcc.specs` / musl 头路径），制品再用 `build/verify-artifact`（`debug/elf`：Machine、ET_EXEC/ET_DYN、PT_INTERP、`ImportedLibraries`）判 `linkage=static`；缺任一证据即退出 1，`ALLOW_UNPROVED_LIBC=1` 只允许 dev 记 `linkage=unproved` 且不写发布 manifest。`build-facts.txt` 逐目标记录 `format/arch/linkage/toolchain/compiler_version/sysroot/musl_evidence/build_env/artifact/smoke`，发布 manifest 由 facts 生成并在写盘前校验每个 linux 目标都是 `linkage=static`。本机冒烟用 `<binary> version`（不是 `--version`），非本机目标记 `smoke=not-runnable-here`。`build/build-linux-release.sh` 把"容器里真实构建 + 独立复核"固化成可重跑的一条命令：容器内 `musl-tools` 构建 linux/arm64，facts 记 `linkage=static`、`musl_evidence=/usr/lib/aarch64-linux-musl/musl-gcc.specs`、`smoke=ok`（`dustkeeper 1.0.0-phase1 (linux/arm64, SQLite 3.53.4, commit f3426fb)`），脚本另用 `readelf` 独立复核得 `machine=AArch64 interp=0 needed=0`。同一脚本也记录了限制：`musl-gcc` 只为自身架构工作，在 arm64 宿主上构建 linux/amd64 会在 C 编译器处失败（`-m64: unrecognized command-line option`），因此脚本默认只构建容器自身架构。用例：`TestAPureGoLinuxBinaryIsRecognisedAsStatic`、`TestTheWrongArchitectureIsRefused`、`TestATextFileIsNotAnArtifact`、`TestTheDarwinPathReadsARealMachO`、`TestReleaseFailsWhenTheLinuxToolchainIsNotMusl`、`TestDevelopmentBuildRefusesToChecksumAnUnprovedArtifact`、`TestReleaseFailsWhenTheArtifactIsNotStatic`、`TestADeveloperCanBuildAnUnprovedLinuxArtifactExplicitly`、`TestReleaseSucceedsWhenEverythingIsPresent`；`--label` 贯通与显式 00:00 由 `TestScheduleRenderHonoursMidnightAndLabel`、`TestScheduleRenderKeepsTheDocumentedDefault`、`TestLabelDrivesTheFileNames`、`TestExplicitMidnightIsHonoured` 覆盖。**仍缺**：linux/amd64、darwin/amd64 未在真机执行，只记 `not-runnable-here` |

### 7.2 R12 的五项整改

1. **必填字段与 absent/null/空集区分**：`strictjson.Inspect` 在 token 层记录每个对象成员的路径与形状（`presence.go`），`rules.ruleFields` 声明 12 个必填字段及其允许形状。`max_size_bytes` 必须出现、值可以是 `null` 或合法整数：上一版把它放进 `optionalFields`，等于把"模型漏写上限"接受成"没有上限"，违反"遗漏不得悄悄扩大匹配范围"的宿主合同。缺失现在是 `INVALID_ARGUMENT`，显式 `null` 才是"无上界"（`TestMaxSizeBytesMustBePresentButMayBeNull`）。
   **同一个对象只认一套精确键名**：`strictjson.Allow` 按每个对象的精确键集拒绝大小写变体和别名重复（`Questions`/`SCOPE_ID` 这类能被 encoding/json 直接绑定的写法），`rule` 对象与 draft envelope 各有一套键集，import 与 draft 走同一个 `checkRuleFields`，所以两条路径不会一个严一个松（`TestParseDraftRejectsCaseVariantsOfItsOwnKeys`、`TestParseRejectsTwoSpellingsOfOneField`、`TestParseAcceptsTheExactFieldNames`）。
2. **draft/needs_input/unsupported 按字段存在性判别**：`forbidFields` 让三种变体互斥到"字段出现即违规"，`questions: []`、`reason: ""`、`dsl: null` 都不再被当成没写。模型草稿的 `dsl` 以 `json.RawMessage` 交给 `rules.Parse`，与 `rule import` 走同一个校验器——测试 `TestModelDraftsGoThroughTheSameFieldContract` 用"删掉 except"这一条证明两条路径同源。
3. **把允许的 scope 列表传给 provider**：`systemWithScopes` 把排序后的逻辑 scope ID 追加到系统提示；超过 64 个时拒绝请求而不是截断（截断等于让模型从看不见的集合里选）。
4. **HTTP 请求体快照**：`internal/rules/testdata/draft_request.golden`，测试对请求体做规范化 JSON 比对，并单独断言"只有两段消息、用户消息等于原文、不含 `/srv/`、文件名、哈希"。
5. **语料解析测试与真实模型评测分离**：`internal/rules/corpus_test.go` 只验证"这些答案能被 DSL 表达、解析器接受"；`test/eval` 才调用真实模型，并把 provider、model、prompt digest、输入、原始输出、宿主决定（含"被宿主拒绝"）逐条写成 JSONL。没有 `DUSTKEEPER_EVAL_BASE_URL` 时它 `t.Skip`，网络调用不是单测前提。

另外发现并修掉一个文档没列、但让 R12 整体无法触达的缺陷：`config` 校验的 provider 名是 `chat_completions_json`，而适配器要求 `openai-compatible`，两个字符串不可能同时满足——**任何通过校验的配置都构造不出 provider**，`rule draft` 在真实二进制上直接 `INVALID_ARGUMENT: provider ... is not supported`。现在两处共用一个常量 `config.ProviderChatCompletionsJSON`，`TestTheDefaultConfigCanConstructAProvider` 用默认配置构造适配器。

**R12 最低验收里还没做到的一条**：「两个年龄阈值」「不是所有」这类语义误解不得直接形成已激活规则。宿主能保证的是"缺字段即拒绝"，不能保证模型没有把双阈值悄悄写成单阈值。为此 `test/eval` 的判定从"kind 相同"改成逐项语义比较（`test/eval/semantic.go`：scope、年龄阈值、大小上下界、match、except、保留期，外加"该澄清时是否澄清、该拒绝时是否拒绝"），并且报告路径 `DUSTKEEPER_EVAL_OUT` 现在是必填——写进 `t.TempDir()` 的报告随进程消失，等于没有证据。`TestTheSemanticGateCatchesWhatTheReviewNames` 用"90 天写成 30 天""丢掉 except""换一个仍合法的 scope""改保留期""凭空加上界""收窄模式"六种偏差证明这个门禁会失败。**但仍然没有真实模型跑过的报告**：这一条只能靠真实 provider 的输出，本轮环境没有可用的端点，因此 A22 记为未交付，不能算关闭。

### 7.3 仍未关闭

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

### 7.4 B5 批次（R13/R14/R15）的可复核入口

```sh
# 诊断日志：字段、上限、轮转、不泄密
go test ./internal/diag ./cmd/dustkeeper -run 'Log|Record' -count=1 -v

# 救援与"不可读≠不存在"
go test ./internal/rescue ./test/integration -run 'Inventory|Unreadable|MissingObjects' -count=1 -v

# 发布产物：真实 musl 证据、静态链接、冒烟
go test ./test/release -count=1 -v
./build/build.sh                     # dev：本机目标 + build-facts.txt
./build/build.sh --release           # 缺目标/缺证据 → 退出 1
./build/build-linux-release.sh       # 容器内用 musl 真实构建 + readelf 独立复核 + 冒烟

# 性能证据（会真的建树，默认不跑）
DUSTKEEPER_BENCH=1 DUSTKEEPER_BENCH_FILES=100000 \
  DUSTKEEPER_BENCH_OUT=docs/evidence/performance-100k.md \
  go test ./test/bench -run TestLargeTreeScanEvidence -count=1 -v
```

`docs/evidence/*.md` 由上面的命令生成（文件头写明 "generated by test/bench; do not edit by hand"），
同目录的 `.json` 是同一份数据，便于脚本比对。

