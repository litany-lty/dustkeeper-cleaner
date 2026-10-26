# dustkeeper 架构

这份文档描述一期实现里**代码实际怎么摆**，以及每条约束落在哪个函数里。设计理由与取舍在实施方案
（`dustkeeper-phase1-implementation-20261025.md`）里，这里只记实现事实：改代码的人应该能在读完这一篇之后
知道哪一层可以动、动了会破坏什么。

---

## 1. 边界：这个程序做什么，不做什么

做：

- 只读元数据扫描，建立候选索引；
- 按**被批准的规则版本**和**单独授予的额度**把文件移进同一卷内的私有隔离区；
- 不覆盖地恢复；
- 到期后按**另一份独立计划**永久删除隔离区里的对象；
- 中断之后按固定矩阵对账，解释不清就停下。

不做（一期硬边界）：

- 不做内容改写、压缩、加密、删除原路径的 unlink；
- 不跟随符号链接（任何一段都不跟随）；
- 不跨文件系统移动（隔离区与被清理目录必须同卷）；
- 不在后台常驻，不调模型做判定（模型只在 `rule draft` 里生成草稿，草稿不构成任何授权）；
- 不触碰系统目录、应用包内部、Time Machine 快照里的内容。

---

## 2. 包结构与依赖方向

```
                      ┌──────────────────────────────┐
                      │  cmd/dustkeeper (CLI 唯一入口) │
                      └───────────────┬──────────────┘
                                      │
        ┌──────────┬──────────┬───────┴───────┬──────────┬──────────┐
        ▼          ▼          ▼               ▼          ▼          ▼
      run ─────► recovery ──► execute ──────► plan ────► policy ──► rules
       │            │           │  │            │                     │
       │            │           │  └──────► scope                  config
       │            │           │              │                     │
       ▼            ▼           ▼              ▼                     ▼
      scan ──────► fsops ◄──── dedup         store ──► migrations   strictjson
        │           │                          │
        └───────────┴──────────► domain ◄──────┘
                                     ▲
                                  errs（叶子）
```

| 包 | 职责 | 允许依赖 |
|---|---|---|
| `domain` | 标识符、身份元组、预算、时钟接口、摘要 | 仅标准库 |
| `errs` | 稳定错误码 → 退出码 / status / retryable | 仅标准库 |
| `strictjson` | 严格 JSON 词法校验 | 仅标准库 |
| `migrations` | 内嵌 SQL，`user_version` 迁移 | 仅标准库 |
| `fsops` | 一切与文件系统打交道的系统调用 | `domain` |
| `store` | SQLite 持久化，单写连接 | `domain`、`migrations` |
| `config` | 路径解析、配置加载与默认值 | `errs`、`strictjson` |
| `rules` | DSL 结构、校验、草稿编译（模型只到这里） | `config`、`domain`、`errs`、`strictjson` |
| `policy` | 匹配判定、全局保护、身份级保护守卫 | `domain`、`rules` |
| `scope` | 注册、owner marker、实例锁、门控 | `config`、`domain`、`errs`、`fsops`、`store` |
| `scan` | 只读遍历与候选入库 | `fsops`、`policy`、`scope`、`store` |
| `plan` | 冻结计划、绑定摘要、清空计划 | `config`、`policy`、`rules`、`store` |
| `execute` | 隔离 / 恢复 / 清空三个原语 | `fsops`、`plan`、`policy`、`scope`、`store` |
| `recovery` | 对账矩阵 | `execute`、`fsops`、`scope`、`store` |
| `dedup` | 精确重复检测（实现 `execute.KeeperVerifier`） | 以上全部 |
| `run` | 编排一次运行 | 以上全部 |
| `schedule` | systemd / launchd 模板渲染 | `errs` |

依赖方向是**单向**的，有两个地方值得单独记住：

1. `execute` **不**导入 `run` 或 `recovery`。执行器不知道"谁在调用它"，它只接受一个 `ScopeCtx`、一个
   `store.Plan`、一个 `store.Item` 和一个预算。这让 `execute` 可以被集成测试直接驱动，也让 `recovery`
   能复用同一个原语来补偿，而不是复制一份移动逻辑。
2. `recovery` 依赖 `execute`，`run` 依赖 `recovery`。**不允许反向**。运行器不是唯一会做对账的地方：
   `recover reconcile` 单独跑也是对账，二者必须是同一段代码。

`execute` 依赖 `plan` 只为 `plan.OperationContractV1` 与 `plan.ItemBindingV1` 这两个**结构**，不调用
planner。`plan` 不需要知道执行器的存在。

---

## 3. 数据模型

全部状态在 `<state>/state.db`（SQLite），文件系统里只有三样东西与状态库对应：

| 位置 | 内容 |
|---|---|
| `<scope>/.dustkeeper/owner.json` | 该目录属于哪个实例、哪个 scope（防止两个实例管同一棵树） |
| `<scope>/.dustkeeper/objects/` | 隔离区对象，名字是对象 ID，不是原文件名 |
| `<scope>/.dustkeeper/manifests/<object>.json` | 移动前落盘的 manifest（身份 + 内容哈希 + 规则摘要 + 计划摘要） |
| `<scope>/.dustkeeper/durability.anchor` | 同卷耐久锚点，darwin 上对它做 `F_FULLFSYNC` |

表（`migrations/001.sql`）：

- `instances` —— 一个状态库对应一个实例。
- `scopes` —— 注册的范围；`kind` 决定可变更性，`status` 是 `active / paused / held`，
  `root_identity` 是注册时用内核身份记下的根，用于每次打开时复核。
- `rules` / `rule_revisions` —— 规则版本是**不可变**的；激活时把 review 摘要绑到版本上，改字段必须产生新版本。
- `grants` —— 每个 `(scope, rule, revision, capability)` 一条，带 `max_files` / `max_bytes`。
  额度为 0 表示禁止，不是无限。
- `plans` / `plan_items` —— 计划是冻结的：`digest` 覆盖 item 集合与绑定，`expires_at_ns` 覆盖时效。
- `items` —— 生命周期对象：原相对路径、父目录身份、计划身份、捕获身份、内容哈希、保留期、状态、`pending_op_id`。
- `quarantine_objects` —— 隔离区里的对象及其清空边界。
- `protections` —— 路径保护与对象保护，`source` 记录它为什么存在（restore / operator / global）。
- `events` —— 追加写入的审计轨迹，每次状态转换一条。

### 3.1 item 状态机

```
        planned
           │  提交 intent
           ▼
        q_intent ──── 移动成功 ────► quarantined ────► p_intent ────► purged
           │                            │
           │ 源还在、隔离区空           │ 提交恢复 intent
           ▼                            ▼
        aborted                     r_intent ──── 恢复成功 ────► restored
                                        │
                                        └── 目标被占 / 解释不清 ──► held
       任意状态 ── 身份不符 / 时钟异常 / 父目录被替换 ──► held
       任意状态 ── 移动已发生但无法验证 ─────────────► unknown（保留 pending 标记）
```

三个不变量写在这里，因为它们是状态机的形状本身：

- **`held` 与 `unknown` 都不会自动变成 `quarantined`**。`held` 的对象永远不会因为"时间到了"变成可清空对象。
  `unknown` 必须经 `recover reconcile` 拿到可解释的观察结果才能离开。
- **每次状态转换都与一个持久化事件同行**。写事件失败就当作转换失败。
- **`aborted` 是终止态**，重放旧计划不会发生：对账只会关闭尝试，不会把旧计划重新执行一遍。

---

## 4. 操作协议：先记账，再动手

`execute.Quarantine` 的注释里写了顺序，代码就是照这个顺序写的：

```
verify → hash → manifest durable → intent commit → rename → verify → sync → commit
   │        │         │                │            │        │        │       │
   │        │         │                │            │        │        │       └ 记 quarantined，
   │        │         │                │            │        │        │          retention 从这里起算
   │        │         │                │            │        │        └ fsync 隔离目录 + 源父目录 + anchor
   │        │         │                │            │        └ 重新哈希，必须与 manifest 一致
   │        │         │                │            └ 排他 rename（不覆盖）
   │        │         │                └ 先写 intent（状态 q_intent，pending_op_id 非空）
   │        │         └ F_FULLFSYNC / fsync 文件、目录、anchor 之后再继续
   │        └ 内容哈希（预算里预留两次读取）
   └ 计划未过期、绑定未变、父目录身份未变、身份+变更令牌未变、nlink==1
```

为什么要这个顺序，而不是"先移再记"：

- 中断点在 `intent commit` 之前 → 文件还在原处，状态还是 `planned`，下次重新计划即可，没有残留。
- 中断点在 `rename` 与最后一次 `commit` 之间 → 隔离区里有对象，manifest 里有它的身份与内容哈希，
  `pending_op_id` 还在，`recover reconcile` 能把这条记录补完（`RECONCILED_OBSERVED`）。
- 中断点在 `manifest durable` 之前 → 没有 intent，什么都没发生。

移动之后才发现问题（`CAPTURE_UNREADABLE`、`CONTENT_CHANGED_AFTER_MOVE`、`DURABILITY_FAILED`、
`RETENTION_OVERFLOW` 等）走 `park()` 而不是 `hold()`：`park()` 把 item 置为 `unknown` 但**保留**
`pending_op_id`，让对账能重新读隔离区。`hold()` 会清掉 pending 标记，用在"文件已经移动、但再对账也没有意义"
的场合；两者混用会造出无人看管的孤儿文件。

`Restore` 与 `Purge` 是同一个协议的两份实例：恢复前先写 `r_intent` 并落盘目标目录身份；清空前先复核授权、
保留期、捕获身份与 keeper，再写 `p_intent`，然后 **只 unlink 隔离区里的名字**——原路径从不交给 `unlink`。

---

## 5. 对账矩阵

`internal/recovery/reconcile.go` 逐行实现实施方案 11.1 的矩阵。观察手段只有两个：`statEntry`（源/目标路径）
与 `statVault`（隔离区对象），都用 fd 相对路径取内核身份，**不比较字符串路径**。

### 5.1 `q_intent`（隔离）

| 源路径 | 隔离区 | 身份判定 | 结论 | 记录 |
|---|---|---|---|---|
| 在 | 空 | — | `aborted`：从未移动，关闭尝试，不重放 | `source still present, vault empty` |
| 空 | 在 | 与预期一致 | `completed`：重新哈希核对 manifest 后补完 | `RECONCILED_OBSERVED` |
| 在 | 在 | 两处都是预期对象 | `held`：单链接准入规则下不应出现 | `DUPLICATE_LINK_STATE` |
| — | 在 | 与预期**不一致** | `held`，scope 置 `held` | `UNEXPECTED_VAULT_OBJECT` |
| 空 | 空 | — | `unknown`：无法解释 | `OBJECT_MISSING_BOTH_PLACES` |
| 在（新文件） | 在 | 隔离区与预期一致 | `completed`，原路径新文件不动 | 附 `left untouched` 说明 |
| 其他 | 其他 | — | `unknown` | `UNEXPLAINED_QUARANTINE_STATE` |

补完时保留期**从对账时刻重新起算**，绝不回填到它无法证明的时刻。这是"不倒推保留期"的落地位置。

### 5.2 `r_intent`（恢复）

| 目标路径 | 隔离区 | 结论 |
|---|---|---|
| 在且身份 == 捕获身份 | 空 | `completed`：恢复已发生，补记录 |
| 空 | 在且身份一致 | `aborted`：恢复没发生，对象仍在隔离区（状态回 `quarantined`） |
| 在 | 在 | `held`（`RESTORE_CONFLICT`）：两边都在，一个字节都没覆盖 |
| 空 | 空 | `unknown`（`RESTORE_UNEXPLAINED`） |
| 其他 | 其他 | `held` |

目标路径来自 intent 的 contract（`raw_relative_b64`），不是 item 的当前字段——恢复目标是**计划时**定下的值。

### 5.3 `p_intent`（清空）

| 隔离区 | 结论 |
|---|---|
| 在且身份一致 | `aborted`：unlink 没生效，回到 `quarantined`，等下一次完整检查 |
| 在但身份不一致 | `held`（`UNEXPECTED_VAULT_OBJECT`） |
| 空 | `completed`，记录 `RECONCILED_ABSENT`——**不声称观察到了一次成功的 unlink，也不声称释放了空间** |

最后一行是刻意的措辞：对象不在，只能说明"它不在了"。审计里不允许出现程序没验证过的断言。

不可解释的状态一律 `MarkItemState(..., StateUnknown, code)`，`unknown` 计数进 `Report`，`recover inspect`
里一直可见，直到有人处理。

---

## 6. 跨平台文件操作

全部集中在 `fsops`。上层**没有任何** `os.Rename`、`os.Remove`、`filepath.Join` 拼出来的路径直接操作系统。
`fsops` 的三个平台文件是 `platform_linux.go`、`platform_darwin.go`、`platform_other.go`，最后一个只返回
`UNSUPPORTED_FS`——不支持就是明确失败，不做降级。

### 6.1 遍历：fd 相对，不跟随符号链接

- Linux：`openat2` + `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS | RESOLVE_NO_XDEV`。
  内核一次性保证"不出根、不跟链接、不跨挂载"。
- macOS：没有 `openat2`，用 `openat(O_NOFOLLOW | O_DIRECTORY)` 逐段下钻。每一段都 `O_NOFOLLOW`，
  因此最后一段是符号链接时打不开，中间任何一段是符号链接也打不开。
- 两种平台都只用 `*at` 系列（`openat` / `fstatat` / `unlinkat` / `renameat`），所以即使目录在检查之后被
  替换成符号链接，也不会跳出去——因为我们握的是**父目录 fd**，路径只在最后一段被解释。

`Root` 记住注册时的 `dev`；每个打开的文件都校验 `st_dev` 与根一致，跨挂载立即 `UNSUPPORTED_FS`。

### 6.2 移动：排他 rename，没有退路

| 平台 | 调用 |
|---|---|
| Linux | `SYS_RENAMEAT2` + `RENAME_NOREPLACE=1`（裸 syscall，不依赖 glibc 版本） |
| macOS | `renameatx_np(RENAME_EXCL)` |

不支持时返回 `UNSUPPORTED_FS`，**绝不**回退到 `rename(2)`（会覆盖）或 `copy + unlink`（不是原子操作，
中途失败会留下半份副本）。这条在代码里没有分支可选，只有"成功"和"报错"。

### 6.3 耐久：文件 → 目录 → 同卷锚点

```
fsync(file) → fsync(parent dir) → F_FULLFSYNC(.dustkeeper/durability.anchor)   [darwin]
                                → fsync(.dustkeeper/durability.anchor)        [linux]
```

darwin 的 `fsync` 只把数据推给设备缓存，`F_FULLFSYNC` 才到介质。anchor 与被移动的文件同卷，所以对它的
全量同步能覆盖该卷上此前所有 rename 的元数据。`ScopeCtx` 私有持有 anchor 的 `*fsops.File` 并暴露
`AnchorFD()`：早期版本用 `os.NewFile(uintptr(fd))` 包了一次，导致同一 fd 有两个所有者，finalizer 提前关闭
后出现 `EBADF`——这个坑现在由"只有一个所有者"从结构上排除。

### 6.4 身份比较，而不是字符串比较

`domain.FileIdentity` = `(dev, ino, size, mtime_ns, ctime_ns, birthtime_ns?)`。所有"这是不是同一个文件"
的判断都走 `Equal`：

- APFS 默认大小写不敏感且做 Unicode 归一化，`Foo.txt` 与 `foo.txt` 是同一个文件，字符串比较会得出相反结论。
  保护判定因此永远拿身份去比（`Guard.CheckExact(rel, id)`），路径保护在遇到无法解析的别名时返回
  `PROTECTION_ALIAS_UNRESOLVED` 并阻断，而不是猜"应该不是同一个"。`run.BuildGuard` 的注释写明了这一点。
- 目录也在比：`SourceParentIdentityJSON` 保证"计划里的父目录"就是"现在的父目录"，被替换过的目录会让计划
  变成 `PLAN_STALE`。

### 6.5 硬链接

`nlink > 1` 的对象一律在**扫描阶段**排除（`scan` 的 `SkippedEntries`），而不是等到执行时再拒绝。删除一个
硬链接不释放空间，而且会让"隔离"变成误导——同一个 inode 还有别的名字在别处。执行器里另有一道
`nlink > 1` 检查，作为计划冻结之后链接数变化的兜底。

### 6.6 两个实现细节，都是被 fuzz 找出来的

- **模式匹配里 `*` 优先于字面比较**。`*` 是合法的文件名字符，模式与名字在同一位置都是 `*` 时，先做字面比较
  会把通配符吃掉，于是 `a*` 不匹配 `a*bc`。分支顺序必须是「先判 `*`，再判字面」，回归用例在
  `TestPatternLanguage` 里。
- **组件长度在 `SplitRelative` 就拒绝**。超过 255 字节的组件内核会在 `openat` 时报 `ENAMETOOLONG`，
  那会以 I/O 错误的形式出现在很久之后，而此时调用方早已认为路径合法。现在它是参数错误。

### 6.7 一棵树只能属于一个 scope

重叠检查对**所有 kind** 生效，不只是可变更的那些。两个 scope 覆盖同一棵树会产生两套候选集，只读那一套
里的过期候选在报告里与真的候选无法区分；拒绝比事后解释便宜。owner marker 的子树探测代价高（最多 20 万
条目），因此只在可变更 kind 注册时运行。

---

## 7. 授权、预算与摘要

三个开关互相独立，没有"全开"的捷径：

| 能力 | 授予命令 | 效果 |
|---|---|---|
| 规则生效 | `rule activate --digest <review_digest>` | 固定版本，旧版本授权作废 |
| 自动隔离 | `rule authorize --auto-quarantine --digest <approval_digest>` | `run` 可以为该版本提交隔离 |
| 自动清空 | `rule authorize --auto-purge --digest <approval_digest>` | 到期对象可以由 `run` 清空 |

授权**不追溯**：`grants` 只在隔离发生的那一刻被消费一次；先隔离后开自动清空，那批旧对象仍需
`purge plan` + `purge apply` 人工清空。

预算语义：`max_files` / `max_bytes` 取 `min(配置默认, grant 批准值)`。`0` 表示**禁止**，永远不表示无限。
执行器在每个 item 之前扣减，扣不动就是 `BUDGET_EXHAUSTED`，本轮停止。

摘要链（`plan/binding.go`）：

```
review_digest    = sha256(purpose + "\n" + canonicalJSON{item 集合 + 绑定状态})
approval_digest  = sha256(purpose + "\n" + canonicalJSON{review 摘要 + capability + 额度 + 保留期})
plan_digest      = sha256(purpose + "\n" + canonicalJSON{plan 字段 + 每个 item 的文档})
```

规范 JSON 用固定 struct 字段序、`SetEscapeHTML(false)`、集合排序去重。**`planner` 与 `verifier` 必须共用
同一个 `itemDocOf` 构造器**——早期 `purge` 自己拼 item 文档，导致 `purge apply` 重新计算摘要时对不上
（`DIGEST_MISMATCH`）。现在 `plan.itemDocOf` 是唯一构造点，purge 计划也不重新编号 ordinal。

---

## 8. 时钟与保留期

`domain.Clock` 提供 `Wall()`、`Mono()`、`BootID()`。

- 回退 > 5 分钟、跳跃 > 48 小时、单次运行内漂移 > 5 分钟 → 清空暂停（`CLOCK_UNTRUSTED`），
  并要求 `doctor --acknowledge-clock` 显式确认。确认动作只写 `clock_ack_ns`，**不修改任何已记录的
  `quarantined_at_ns` / `purge_not_before_ns`**——不确定的时间不能被一次确认抹平。
- `purge_not_before = quarantined_at + retention`，两端都用整数纳秒，`domain.AddSecondsNS` 溢出即失败。
- 保留期只能来自被批准的 DSL（生命周期对象）或宿主的固定 30 天（精确重复对象，显示在计划里并计入摘要）。

---

## 9. 并发与锁

| 锁 | 位置 | 作用 |
|---|---|---|
| 实例锁 | `<state>/instance.lock` | 整个状态库同时只有一个写者（`INSTANCE_BUSY`，exit 3） |
| 控制锁 | `<state>/control.lock` | `pause` / `resume` 与运行互斥 |
| 注册锁 | `~/.dustkeeper-registry.lock` | 注册可变更 scope 时串行化，避免两个状态库抢同一棵树的 owner marker |

注册锁刻意放在 home 下、与状态目录无关：改 `DUSTKEEPER_STATE_DIR` 不能造出两把互不相干的锁去管同一棵树。

SQLite 本身用单写连接（`SetMaxOpenConns(1)`）、`journal_mode=DELETE`、`synchronous=EXTRA`、
`foreign_keys=ON`、`busy_timeout=1000`、`mmap_size=0`、darwin 上 `fullfsync=ON`。不用 WAL、不用 mmap，
因为这两个都会在断电时引入"已提交但没落盘"的窗口。

---

## 10. 明确不做的事，以及为什么

| 没做的事 | 理由 |
|---|---|
| 不跟随符号链接 | 跟随会让"删除这个文件"变成"删除链接指向的东西"，而计划里写的是链接 |
| 不跨卷移动 | 跨卷只能 copy+unlink，不是原子操作；半途失败会留下两份 |
| 不用覆盖式 rename | 覆盖会毁掉目标位置的文件，而目标位置的文件可能正是用户要救的那一份 |
| 不自动修复 `held` / `unknown` | 自动修复意味着用推测填补观察不到的事实 |
| 不在 `run` 里删除任何东西 | 清空只能由 `purge apply` 发起，`run` 也只走同一条受检路径 |
| 不让模型参与判定 | 模型输出只能是 `draft` / `needs_input` / `unsupported`，进库前要过 DSL 校验，激活还要人工摘要 |

---

## 11. 已知限制（一期）

- 只支持 Linux 与 macOS；其他平台 `init` 之后第一个 `doctor` 就会返回 `UNSUPPORTED_FS`。
- 交叉编译需要目标平台的 C 编译器（cgo）；`build/build.sh` 缺编译器时跳过该目标并打印原因，不静默产出错误产物。
- 隔离区在被清理的目录内部（`<scope>/.dustkeeper/objects`），因此 `scan` 显式跳过 `.dustkeeper`
  （`scan.vaultName`）；这在一期是可接受的取舍（同卷保证排他 rename 可用），代价是清理整棵目录时会连同
  隔离区一起被带走。
- `dedup` 的 keeper 选择是保守的：元数据不一致就报告"保留"，不做近似判定；跨 scope 提升 keeper 只在
  提供 `--keeper-scope` 时发生。
- `run` 的磁盘压力门槛（`MinFreeBytes` / `MinFreePercent`）尚未接入编排器。配置项已经存在并通过校验，
  但运行时**没有**依据剩余空间提前停止；目前只有 `CLOCK_UNTRUSTED` 与预算耗尽会中止一轮。
- `doctor --acknowledge-clock` 只记录确认时刻，**没有**把无法证明保留期的对象 `purge_not_before` 延后到
  "确认时刻 + 完整保留期"（文档 §11.2 的要求）。确认后到期对象会立即可清空。
- 没有运行日志子系统：`logs/` 目录会被创建和检查，但没有写入与 5 MiB × 5 轮转（文档 §13.2）。
- `recover inspect` 依赖数据库；`--manifests` 只读恢复清单尚未实现（文档 §8.3）。
- `internal/scan`、`execute`、`recovery`、`dedup`、`run` 没有包内单测，主路径由 `test/integration` 覆盖。

逐条的文档对照与仍缺的验收用例见 [`docs/compliance.md`](compliance.md)。
