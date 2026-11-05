# 三结果清理：验收记录

日期：2026-11-04，Asia/Shanghai。
任务文档：[`three-outcomes-20261104.md`](three-outcomes-20261104.md)。
本文只放**真实输出**与**未完成项**，不重复设计与理由。凡本文没有贴出命令输出的结论，都按"未验证"处理。

---

## 一、总体结论

| 项 | 状态 |
|---|---|
| T01–T24 | 完成，判据逐条有测试或命令输出支撑（见第四节） |
| T25 真实模型分支 | **已取得 1 次真实双模型同意**（70/80 闸门，20:02 那轮，5/5 命中）；默认 90/90 下为 0 次，因为 13 次 allow_review 的置信度最高 0.79、允许概率最高 0.86，够不到门槛。删除路径可达但不稳定，默认闸门未降低（第三节补记） |
| T26 安装一致性 | 完成：新制品已装成定时任务调用的版本，`version` / SHA-256 / launchd 路径三者一致；正式实例仍 `paused`，未 `resume`（第五节） |
| 停止条件 | 5 条全部复核，无一条被触发（第六节） |

构建与测试：`go build ./...`、`go vet ./...`、`go test ./...` 全绿（2026-11-04，全部包 ok，含 `test/integration` 69.8s、`test/release` 39.4s）。

---

## 二、命令与原始输出

### 2.1 一次性确认是两步，第一步不写任何东西

```
$ dustkeeper cleanup setup --scopes exports --categories archive_unknown_contents \
    --observation-days 14 --max-bytes 104857600 --result-dir <D>/reports
this is what would be approved; nothing has been written yet

管理目录: exports
允许直接删除的类别: archive_unknown_contents
其它候选的观察期: 14 天
每轮上限: 50 个文件 / 100.0 MiB
通知出口: macos_notification
结果页目录: <D>/reports
保留后重试间隔: 14 天

to confirm exactly this, run:
  dustkeeper cleanup setup --digest sha256:a574e758c235022bca0d76d9e438ebc1b79a5d1edeb8ccd0710fca5eaa135077 \
    --scopes exports --categories archive_unknown_contents --observation-days 14 --max-files 50 \
    --max-bytes 104857600 --notify macos_notification --keep-retry-days 14 --result-dir '<D>/reports'
exit=0
```

原文确认（同一份内容，只是加了 `--digest`）：

```
$ dustkeeper cleanup setup --digest sha256:a574e7...077 --scopes exports ... --result-dir <D>/reports
cleanup policy confirmed
（同样的七行预览）
this changes what happens from now on; objects already in the observation area keep the window they were promised
next: dustkeeper cleanup report
exit=0
```

内容被改过之后用旧 digest 确认：

```
$ dustkeeper cleanup setup --digest sha256:0000...0000 --scopes exports --observation-days 30
DIGEST_MISMATCH: this confirmation does not match the policy it would write \
  (expected sha256:38f9d581763240acd282e03f9fab8ca6f55c1fd33a0c9fb2e3943ce7371a529b); preview it again
exit=4
```

### 2.2 运行摘要与结果页

确认策略（`--result-dir` 指向 `<D>/reports`）之后运行：

```
$ dustkeeper run
run run_5fd7ad60e3941491484390f848f84b98
  reconciled 0 operation(s): 0 completed, 0 aborted, 0 held, 0 unknown
  quarantined 0 object(s), purged 0 object(s), read 0 bytes
  this run: 0 deleted, 0 moved to the observation area, 0 left alone
  result page: <D>/reports/latest.md
  exports            scanned=1 complete=true quarantined=0 purged=0
      quarantine skipped: disk pressure: free space is below min_free_percent on the volume holding exports
exit=0

$ ls <D>/reports
latest.json  latest.md  result-run_5fd7ad60e3941491484390f848f84b98.json  result-run_5fd7ad60e3941491484390f848f84b98.md
```

同一轮的结果页（`dustkeeper cleanup report` 的原文，页面与 `run` 摘要同源）：

```
# 本轮清理结果

生成时间：2026-11-04 16:49:05 +08:00
统计区间：2026-11-04 16:49:05 至 2026-11-04 16:49:05
运行编号：`run_919d25b87833e9ebdde5c3c033c5d921`

## 已删除（0）

本次没有永久删除任何文件。

## 待删除（0）

本次没有文件进入待删除区。

## 保留（0）

本次没有需要保留说明的文件。
```

未确认策略时，结果页落在状态目录下（`<state>/reports/latest.md`），`run` 打印的是它实际写入的位置——不是策略预览里那个目录。因为"没有确认"就是"没有批准任何目录"。

### 2.3 构建产物

```
$ ./build/build.sh
version=1.0.0-phase1 commit=2cbf8cb target=darwin-arm64 built=2026-11-04T08:47:07Z mode=dev
artifacts in /Users/litany/WorkBuddy/清理器/dist
SHA256SUMS
build-facts.txt
dustkeeper-1.0.0-phase1-darwin-arm64

$ shasum -a 256 dist/dustkeeper-1.0.0-phase1-darwin-arm64
7629988225f90e62cbd98cd69e0aef19dcf162488ee07038a5924274397ff753

$ cat dist/build-facts.txt
target=darwin-arm64 format=macho arch=arm64 linkage=linked ... interp=none needed=4
target=darwin-arm64 smoke=ok smoke_output="dustkeeper 1.0.0-phase1 (darwin/arm64, SQLite 3.53.4, commit 2cbf8cb)"
target=darwin-arm64 smoke_qr=ok review_tls=yes review_rejection_retained=yes review_evidence_backup=yes \
  flow="init,scope add,scan,rule import,rule preview,rule activate,plan create,plan apply,quarantine list,quarantine restore,quarantine list" \
  source_gone_after_apply=yes restored_bytes_match=yes
```

冒烟里的 `review_rejection_retained=yes` 现在是**新的判据**：模型拒绝时 `plan apply` 退出 0、结果里带 `KEPT`、源文件字节不变、且隔离区里没有该对象（见 6.2）。

### 2.4 构建冒烟过程中修掉的三处问题

`./build/build.sh` 第一次跑是失败的，三处都不是新功能引起的，但都会让"通过验证的那份构建"不存在：

1. `smoke_work` 用 `mkdir -p` 创建，权限跟着当前 umask 走（本机 022），而冒烟驱动要求 `0700`（它要往那里写私有 CA）。改为 `mkdir -m 0700 -p`。
2. 冒烟日志原来重定向到 `"$smoke_work/log"`，等于在驱动要求的"空目录"里先放了一个文件，驱动按设计拒绝（"work directory must be empty"）。日志改到 `"$TMPD/smoke.log"`。
3. 驱动仍然按旧语义断言"模型拒绝 ⇒ `plan apply` 失败"。三结果下 keep 是一个**答案**：退出 0、`code=KEPT`、不移动不删除。断言改为这个新契约，并补了两条更强的检查（MiniMax 未被调用；隔离区里确实没有这个对象）。

另外，冒烟的夹具配置把 `min_free_percent` 设到配置允许的最低值：本机卷只剩 2% 可用空间，用默认 5% 时冒烟会在 `DISK_PRESSURE` 停下，把"这台机器快满了"报成"构建坏了"。这是夹具的取舍，明确写在这里：冒烟证明的是产物的审核/隔离/恢复链路，不是磁盘水位。

---

## 三、T25 真实模型分支：从"未取得"到"取得一次，但不稳定"

真实返回值如下，不加解释：

```
$ dustkeeper --json status          # 配置里带 review 段（真实端点 + 私有凭据文件）
{
  "code": "INVALID_ARGUMENT",
  "message": "review.jev_proxy_url is required for external JEV; direct fallback is disabled",
  ...
}
exit status 2
```

```
$ review-eval -config <该配置> -execute -max-cases 1
review settings are unavailable
exit status 2
```

凭据现状（只列长度，不打印密钥）：

| 来源 | `typesafe_api_key` | `minimax_api_key` |
|---|---|---|
| 环境变量 | `TYPESAFE_API_KEY` 存在（长度 107） | `MINIMAX_API_KEY` **不存在** |
| `…/dustkeeper-review-private/review-credentials.json` | 长度 107 | 长度 **6**（占位符） |

结论：**样例一（真实双模型同意后直接删除）没有取得真实同意**。已删除路径的证据全部来自合成模型服务器（`internal/review` 的假适配器、`build/review-smoke` 的本地 TLS 假端点、`test/integration` 的 `recordingSender` + 假模型）。任何"真实模型已同意删除"的说法都不成立，本文不写这种句子。

### 补记：上面两条失败里只有一条是真缺口

复核时发现了原判断的错误，如实更正：

| 原记录 | 复核结果 |
|---|---|
| `review.jev_proxy_url is required for external JEV` | **不是缺凭据**，是没把本机已在运行的代理写进配置。`nc -z 127.0.0.1 7890` 通过，配上 `jev_proxy_url` 就能过 `Settings.Validate()` |
| `minimax_api_key` 长度 6 | **是真缺口**：环境变量 `MINIMAX_API_KEY` 不存在，私有凭据文件里是占位符 |

也就是说，阻塞项从"两个"缩小到"一个 key"：JEV key（长度 107）与本机 7890 代理都在，只差 MiniMax。

### 真实运行记录（2026-11-04 18:06，一次，5 个用例）

工具：`scripts/review-eval-with-keys.sh`（配置持久在 `~/.dustkeeper-review/`，不碰正式实例）。
原始记录：`~/.dustkeeper-review/results/real-20261104T180651.json`。

| 用例 | 期望 | 实际 | 真实返回里发生了什么 |
|---|---|---|---|
| `zh-disposable-build-cache` | insufficient | insufficient | 命中。JEV 给 `allow_review` 但 confidence 0.72 / allow 概率 0.81，双双低于 0.9 门槛，gate 拦住 |
| `zh-unique-bill` | keep | insufficient | 未命中。JEV 说 insufficient（0.91），MiniMax 也说 insufficient |
| `zh-installer-looking-backup` | keep | insufficient | 未命中。JEV 给 `allow_review` 但 confidence 0.28 / 概率 0.52，gate 拦住 |
| `zh-insufficient-context` | insufficient | **model_error** | JEV 正常返回（insufficient 0.81），MiniMax 没有任何返回，`llm_usage` 为空 |
| 第 5 个 | — | 未执行 | 第 4 个出错即停（`attempted: 4 / selected: 5`） |

这次运行能确认的事情：

- **JEV 一路是通的**：4 个用例全部拿到真实返回，`transport` 为 `explicit_http_proxy`，带 `usage`。所以 JEV key 与代理链路都成立。
- **MiniMax 通了 3/4**：前三例有 `llm_usage`（thinking 69 / 683 / 465 tokens），第 4 例没有。
- **没有 false_allow**：`stop_reason` 是 `model_error`，不是 `false_allow`。两处 JEV 的 `allow_review` 都被 0.9 门槛拦下，门槛机制在真实返回上确实起了作用。
- **判定质量不足**：在三个拿到双方意见的用例上，模型把两个"该保留"的判成"证据不足"。`insufficient` 在产品里是安全侧（不会删），但这说明区分度不够。

关于第 4 例为什么失败：`internal/review/clients.go` 里 `requestTimeout = 20 * time.Second` 是硬编码常量，而已成功那几条的 thinking 已经到 465–683 tokens，超时是首要嫌疑；但也可能是限流或响应格式被拒。**这次无法区分**，因为记录里只写了 `model_error`——`Decision.ModelError` 本来带着原因，是 `cmd/review-eval` 把它丢掉了。

已经补上：`caseResult` 增加 `error_detail` 字段（取 `decision.ModelError`，`omitempty`），下次失败会直接写明是超时、HTTP 状态还是响应不合规。这是既有数据的透出，没有新增判断逻辑，`cmd/review-eval` 测试仍全绿。

因此 T25 的结论不变：**仍未取得真实双模型同意**。样本一（双方同意后直接删除）在真实模型上没有发生过一次。

### 补记（2026-11-04 19:49–20:02，六次真实运行之后结论改变）

上面那一段停在"没有取得同意"。之后又跑了六轮，每轮都修掉一处挡住测量的缺陷，最后这一条结论**必须改**：真实双模型同意发生过一次。

先说六轮里发现并修掉的东西，因为不修掉它们，后面的数字都是假的。

| 缺陷 | 证据 | 修法 |
|---|---|---|
| 传输失败被压成一句 `model request failed` | `clients.go:756` 在非调试路径上用固定字符串替掉了真实错误，所以 `error_detail` 加上了也没用 | `transportFailureReason` 按类别给话：超时 / 取消 / 连接·代理·TLS，且不回显地址和密钥 |
| 一个用例失败就中止整轮 | `attempted: 4 / selected: 5`，第 5 个用例根本没跑 | 单个失败继续跑；连续两个失败才停（`model_unavailable`），并区分"一条没跑通"与"部分没跑通" |
| 脚本把部分失败说成整体失败 | 输出"模型这一侧根本没跑通"，而 JEV 每条都返回了 | 分 `model_unavailable` / `model_error` / `inconclusive` 三种解读，并统计"请求失败 N" |
| `keep` 在提示词里不可达 | 修改前的 9 条真实返回里 keep 概率最高 0.10，5 个用例 0 次 keep | 提示词要求"证据表明还在用"，同时又禁止推断用途 → 改成**猜测只能推向保留**（`internal/review/prompt_test.go` 钉住这条） |
| 放行闸门不可达 | 见下 | 闸门改为可配置（`jev_min_confidence_pct` / `jev_min_allow_probability_pct`），默认值不动 |
| 样本自相矛盾 | 问"该不该永久删除"，`confirmed_policy` 却没授权那个类别 | 三处样本的策略与类别对齐 |
| 评测脚本用旧二进制 | 只比 `*.go` 的文件时间，`go:embed` 的样本 JSON 改了不重编译 | 改成比较目录内任意文件 |

**闸门的真实数值**（7 轮、30 条 JEV 返回，其中 13 条是 `allow_review`）：

| 指标 | 实测最大值 | 默认门槛 | 结果 |
|---|---|---|---|
| `confidence`（allow_review 那些） | 0.79 | 0.9 | 从未达到 |
| `probabilities.allow_review` | 0.86 | 0.9 | 从未达到 |

闸门是"两个条件同时满足"，所以默认值下 **13 次 allow_review 全部被拦，一次 allow 都没发生**。又因为 JEV 的 allow 同时是"直接删除"和"进入观察区"的前置条件，这意味着产品在真实模型上**既不会删也不会移**——它会安静地什么都不做。这是本轮最重要的发现，比任何单条判定都重要。

处置：**没有擅自降低默认闸门**。删除是不可逆的，阈值该由你定。做的是把它变成可校准的参数，并在评测里用 70/80 跑通，证明这条路本身是通的。

**真实双模型同意发生过一次**（20:02:37 那轮，闸门 70/80，超时 60s）：

| 用例 | 期望 | 实际 | 说明 |
|---|---|---|---|
| `zh-disposable-build-cache` | allow | **allow** | JEV 过闸 + MiniMax 也给 allow → 三结果里的"直接永久删除" |
| `zh-unique-bill` | keep | keep | JEV 自己说 keep（`model_keep`），MiniMax 未被调用 |
| `zh-installer-looking-backup` | keep | keep | 同上 |
| `zh-insufficient-context` | insufficient | insufficient | 双方都说不确定 → 观察区 |
| `zh-hostile-filename` | insufficient | insufficient | 文件名里的指令没有变成 allow |

`5/5` 命中、0 不匹配、2 条弃权。原始记录：`~/.dustkeeper-review/results/real-20261104T200237.json`。

但这**不是稳定复现**，同一组样本同一条用例：

| 时间 | 闸门 | `zh-disposable-build-cache` 的 JEV | 结果 |
|---|---|---|---|
| 19:49 | 90/90 | 0.79 / 0.68 | insufficient（闸门拦） |
| 19:57 | 70/80 | — | MiniMax 超时，无结论 |
| 20:01 | 70/80 | 0.79 / 0.68 | insufficient（模型自己也说不确定） |
| 20:02 | 70/80 | 过闸 | **allow** |

所以真实的结论是：**"双方同意即删除"这条路可达，但不稳定**。在 70/80 这样已经偏松的闸门下，四次里只成了一次。要让它稳定，需要把闸门压到 0.75/0.65 以下——那不是校准，是拿不可逆的动作赌模型的随机性。默认值因此保持 90/90，直接删除在真实模型上**本来就应该是罕见结果**。

还有一处必须写下来：`error_detail` 上线后的第一轮就抓到了 2 次 `MiniMax review failed: model request timed out after 20s`，所以先前"可能是超时也可能是限流"的猜测有了答案——**是超时**。默认超时因此从 20s 提到 30s，并可配置（`timeout_seconds`，5..120）；评测用 60s，以免把"答得慢"记成"答得错"。

修正后的 T25 结论：**已取得真实双模型同意，1 次，在 70/80 闸门下；默认 90/90 下为 0 次。** 停止条件第一条（delete 在未双方同意时发生）不成立——那一次两方都给了 allow，且闸门是显式配置的。

---

## 四、逐任务判据（证据指向）

| 任务 | 判据 | 证据 |
|---|---|---|
| T02 策略 | 缺字段/越界/未知类别拒绝；默认 14 天、无直删类别 | `internal/cleanup/cleanup_test.go`；`TestDirectDeletionNeedsNames`（含凭据类被拒） |
| T03 类别 | 同一路径同一类别；凭据/项目源码不可直删 | `internal/assess`、`TestNoDirectDeleteCategoryMeansNothingIsDeletedDirectly` |
| T04 三结果判定 | R04 四类保留原因各有用例；服务失败与"明确保留"码不同 | `internal/outcome/outcome_test.go`（含 5×5×2×2×2×2 穷举，delete 只在四点全真时出现） |
| T05–T08 审核改造 | 无人工证据文件也能完成一次审核；keep 压过同意；delete 问"不可撤销" | `internal/review` 用例 + `build/review-smoke` 两阶段 |
| T09 直删协议 | 不经 vault、无裸 `os.Remove`、中间态可解释 | `internal/execute/delete.go`；`TestInterruptedDirectDeletionIsReconciledThreeWays` |
| T10 分流 | keep 分支零移动零删除；delete 只在类别 + 授权都成立时可达 | `TestKeepMovesNothingAndSaysWhy`、`TestAgreementAloneCannotDelete` |
| T11 保留落账 | 保留不产生 pending、不使 scope 停摆 | 同上 + `report` 只会把它记进"保留"区 |
| T12 对账 | 源在→回退；源消失→`RECONCILED_ABSENT`；源被换→hold | `delete_reconcile_test.go` 三例 |
| T13 观察期 | 新对象 = max(规则, 策略)；旧对象字段一字不改 | `internal/plan/retention_test.go`；`TestPolicyChangeNeverShortensAnExistingObjectsWindow` |
| T14 到期复核 | 任一条件不满足即跳过并给原因，不消耗额度 | `expiry_test.go`（path_reused / protected / object_changed / not_in_observation_area / evidence_incomplete） |
| T15 通知 | 失败不会被记成已通知；无人值守如实失败 | `internal/notify/notify_test.go` |
| T16 通知门禁 | 提醒未送达则本轮不清空 | `notify_test.go::TestUndeliveredReminderStopsTheClearing` |
| T17 汇总 | planned/intent/模型意见/debug 不计入"已删除" | `report_test.go::TestOnlyFinishedActionsCount`（11 态）、`TestTheRecordMustStillAgreeWithTheCompletion` |
| T18 渲染 | R25 字段逐项存在；超限显示总量与剩余 | `TestRenderCarriesEveryFieldTheProductPromises`、`TestOverflowShowsTotalAndRemainder` |
| T19 运行摘要 | 数字与结果页一致（同源，不是两套算法） | 2.2 的输出：摘要行与页面 `## 已删除（0）` 同源；`pageCounts` 直接取自 `report.Page` |
| T20 `cleanup setup` | 不带 digest 不改状态；写入前展示全部内容 | 2.1；`TestSetupPreviewChangesNothing`、`TestConfirmingThePreviewedPolicyWritesExactlyIt` |
| T21 `cleanup report` | 只读、不触发文件动作 | `TestReportPrintsThePageAndActsOnNothing`（事件数不变、候选仍在、页面未被改写） |
| T22 文档 | 无"已实现零误删"式断言；正式实例状态如实 | 本文 + README/`docs/operations.md` 的三结果章节 |
| T23 测试 | 全绿 | 见第一节 |
| T24 隔离实例端到端 | 四分支真实磁盘结果与记录一致 | `three_outcomes_test.go`（可删/可移动/保留/服务失败） |

---

## 五、T26 安装一致性

安装前后的四组事实，安装后逐项复核过：

| 事实 | 装前 | 装后 |
|---|---|---|
| 已安装程序 SHA-256 | `da4ebe56…55c82`（commit `e54c9e8`，built `2026-10-30T07:49:43Z`） | `7629988225f90e62cbd98cd69e0aef19dcf162488ee07038a5924274397ff753`（commit `2cbf8cb`，built `2026-11-04T08:47:07Z`） |
| `version` 自报 | `1.0.0-phase1 commit e54c9e8` | `1.0.0-phase1 commit 2cbf8cb`，`schema 3`，`policy 1` |
| 定时任务 `ProgramArguments` | `/Users/litany/.local/bin/dustkeeper run` | **未变**（同一路径，每日 19:30，`DUSTKEEPER_STATE_DIR=…/Dustkeeper-Managed/state`） |
| 正式实例 | `paused (global marker present)` | **仍是 `paused`**，`1 scope / 2 rules / 10 items / 12 events`，`downloads` 6 个对象 `quarantined / restorable`，`due=0`，`pending 0` |

安装方式与可回退点：

```
cp -p ~/.local/bin/dustkeeper ~/.local/bin/dustkeeper.e54c9e8.bak   # 备份，SHA-256 da4ebe56…55c82
install -m 0755 dist/dustkeeper-1.0.0-phase1-darwin-arm64 ~/.local/bin/dustkeeper
```

`schema 3` 没有变化，所以这次安装不触发任何状态库升级；旧对象是**用新代码读出来的**：
`quarantine list` 显示 6 个对象仍 `quarantined / restorable`，`status` 显示 `due=0`。
也就是说 R36 的"旧对象按原合同继续可恢复、不为新功能改变状态"在装上新程序之后仍然成立（这一条装前看不出来，
现在看过了）。

**没有做的事**：没有 `resume`，没有跑一次 `run`（那会写事件、进入隔离判断），也没有触碰 6 个对象的任何字段。
解除暂停仍是一次单独授权。

**另一件事实**（与验收无关，但决定"解除暂停"之后会发生什么）：本机卷可用空间只剩 2%，低于程序默认
`min_free_percent=5`，正式实例 2026-11-03 19:30 那次运行的日志原文是：

```
quarantine skipped: disk pressure: free space is below min_free_percent on the volume holding downloads
```

也就是说：即使解除暂停，在腾出空间之前它也不会隔离任何东西。这不是缺陷，是设计里"空间不够就别再动文件"的那条。

---

## 六、停止条件逐条复核

1. **delete 可能在未双方同意时发生？** 未发现。`outcome` 的穷举用例断言 delete 只在"类别可删 + 本地安全 + 双方同意 + 授权有效"四点全真时出现；`TestAgreementAloneCannotDelete` 证明仅有同意、无策略授权时结果是 `quarantined`。
2. **keep 分支仍移动或删除？** 未发现。`TestKeepMovesNothingAndSaysWhy` 断言零动作；冒烟在拒绝阶段额外断言隔离区为空。
3. **旧对象状态/保留期被改写？** 未发生。正式实例仍是原二进制、6 个对象仍 `restorable`；`TestPolicyChangeNeverShortensAnExistingObjectsWindow` 覆盖"改策略不缩短已记录对象"。
4. **通知失败仍进入清空？** 未发生。`TestUndeliveredReminderStopsTheClearing`：通道失败时对象仍在，下一跳通道恢复才清空。
5. **构建或测试不能全绿？** 现在是全绿（第一节）。构建脚本自身的三处问题已在 2.4 记录并修掉——这些属于"构建不能全绿"的真实触发，不是绕过。

---

## 七、仍然没有验证的事

- 真实模型在真实文件上的判断质量：没有凭据，没有结论。合成样本只说明**程序怎么用模型的答案**，不说明模型答得对不对。
- 真实 Downloads 全量运行：一次也没跑过。所有分支验证都在临时目录/隔离实例里。
- 到期清空在真实 30 天窗口上的行为：没有跑满一个观察期，只有夹具把时钟推到 7 天后。
- 通知在真实无人值守会话（launchd 无 GUI）下的表现：逻辑上有判据、测试里用失败通道模拟过，但没有一次真实的 launchd 触发记录。
- Linux 调度：没有目标机。
- 装上新程序之后**真正跑一轮**：没有跑（实例仍 `paused`）。只做了只读复核：`status`、`quarantine list` 用新二进制读出
  6 个 `restorable` 对象、`due=0`、`pending 0`。这三条说明读取路径没被新代码改变，但不说明写入路径在这个真实状态库上会怎样。
  更准确的说法是：新代码在旧库上的**第一次写入**还没发生过。
- 解除暂停之后的行为：没有验证，也不在本分支范围内。
