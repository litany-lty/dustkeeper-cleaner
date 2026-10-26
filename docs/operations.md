# dustkeeper 运维手册

面向要在一台真实机器上把它装起来、跑起来、并在出问题时知道该看哪里的人。

装之前先读这一句：**这个程序的第一版故意把"能自动做的事"压到很小**。装上之后它会一直在做"扫描 + 报告"，
除非你逐项打开规则激活、自动隔离、自动清空这三个开关。如果你只想要一个只读的观察者，装完不做第 5、6 步
就可以，它不会移动任何文件。

---

## 1. 前置条件

| 项目 | 要求 |
|---|---|
| 平台 | Linux（x86_64 / arm64）或 macOS（arm64 / x86_64）；其他平台会在 `doctor` 报 `UNSUPPORTED_FS` |
| 文件系统 | 与被清理目录同卷的普通本地文件系统。需要支持排他 rename（Linux `renameat2`、macOS `renameatx_np`）；网络卷、FUSE、部分容器 overlay 不支持 |
| 内核 | Linux ≥ 5.6（`openat2`）；以下版本会在打开 scope 时失败，而不是降级 |
| 权限 | 以**要清理的文件的所有者**身份运行。不要用 root 去清理用户目录 |
| 磁盘 | 状态库与隔离区都占用空间。隔离区里放的是原文件本身，按"最多保留 N 天 × 每日隔离量"预留 |

一期不支持：跨卷移动、跟随符号链接、清理系统目录或应用包内部。

---

## 2. 安装

```sh
tar xzf dustkeeper-1.0.0-phase1-<os>-<arch>.tar.gz     # 或直接复制二进制
install -m 0755 dustkeeper /usr/local/bin/dustkeeper
dustkeeper version
```

核对下载文件（发布目录里的 `SHA256SUMS` 是在 `dist/` 内生成的相对路径）：

```sh
cd dist && shasum -a 256 -c SHA256SUMS
```

macOS 上 `build/build.sh` 产出的二进制已做 ad-hoc 签名（`codesign --force --sign -`）。如果你自己重新编译，
也用同样的方式签一下，否则每次执行都会被系统重新评估，首次运行会明显变慢。

---

## 3. 初始化

```sh
dustkeeper init
```

创建（权限 `0700`）：

| 路径 | Linux | macOS |
|---|---|---|
| 配置目录 | `$XDG_CONFIG_HOME/dustkeeper` 或 `~/.config/dustkeeper` | `~/Library/Application Support/Dustkeeper` |
| 状态目录 | `$XDG_STATE_HOME/dustkeeper` 或 `~/.local/state/dustkeeper` | `~/Library/Application Support/Dustkeeper` |
| 数据库 | `<state>/state.db` | 同左 |
| 日志目录 | `<state>/logs` | 同左 |

`DUSTKEEPER_CONFIG_DIR` / `DUSTKEEPER_STATE_DIR` 可以覆盖以上路径。**同一台机器上跑多个实例时**必须分别
设置——但请注意 owner marker 仍然会阻止两个状态库管理同一棵目录树。

注册时还会在 `$HOME` 下创建 `.dustkeeper-registry.lock`（注册锁）。它故意放在 home 而不是状态目录：
换状态目录不该造出两把互不相干的锁去抢同一棵树。

---

## 4. 注册第一个范围

```sh
dustkeeper scope add --path /srv/app/disposable-exports --kind managed_disposable --name exports
```

`--kind` 的四种取值：

| kind | 可变更 | 用途 |
|---|---|---|
| `observe` | 否 | 只扫描、只报告 |
| `assisted` | 是 | 需要人工逐个批准（一期通过 `plan create` + `plan apply`） |
| `managed_disposable` | 是 | 唯一可以被自动隔离、并在到期后被自动清空的类型 |
| `reference` | 否 | 查重时的"正本"目录，永不移动 |

`managed_disposable` 会在目录内创建 `.dustkeeper/`： `owner.json`（所有权标记）、`objects/`（隔离区）、
`manifests/`（移动前落盘的清单）、`tmp/`、`durability.anchor`（耐久同步锚点）。

**注册不等于授权。** 到这里为止，程序已经可以扫描这个目录，但还不会移动任何东西。

一棵树只能属于**一个** scope：同一路径重复注册、或者注册一个已被别的 scope 覆盖（包含或被包含）的目录，
都会被拒绝（`SCOPE_OVERLAP`）。这条对只读 scope 也生效，因为两套候选集会让报告里的过期候选与真的候选
无法区分。真要分开管理，就注册更窄的目录。

```sh
dustkeeper scope list
dustkeeper scope show --scope exports
```

---

## 5. 上线前体检

```sh
dustkeeper doctor
dustkeeper doctor --scope exports
```

六项检查及常见处置：

| 检查项 | 失败含义 | 处置 |
|---|---|---|
| `state_dir` | 状态目录不存在或权限不是 `0700` | `dustkeeper init`；或修正权限 `chmod 700` |
| `database` | 数据库打不开或 pragma 回读不一致 | 确认磁盘可写、文件未损坏；必要时从备份恢复 |
| `capabilities_state_dir` | 该卷不支持排他 rename 或 fsync 语义 | 换到本地卷；不要在网络盘 / FUSE 上使用 |
| `pause_marker` | 存在全局暂停标记 | 确认原因后 `dustkeeper resume` |
| `pending_operations` | 有未对账的操作 | `dustkeeper recover inspect`，见第 9 节 |
| `clock` | 墙钟回退过大或跳跃过远 | 校准系统时间后 `dustkeeper doctor --acknowledge-clock` |

每个 scope 还会额外做一次"根目录身份复核 + 能力探测"。根被替换过（重装系统、恢复备份、`mv` 过目录）
会报 `SCOPE_CHANGED`：这不会自动修复，需要重新注册该范围。

`--acknowledge-clock` 只记录"当前墙钟可信"，**不会**修改任何已存对象的 `quarantined_at` / `purge_not_before`。
不确定的时间不能被一次确认抹平，这是刻意的。

---

## 6. 规则：从草稿到授权

规则的生命周期是四步，每一步都有自己的检查点：

```sh
# 1) 导入（或让模型写草稿，草稿不构成任何授权）
dustkeeper rule import --file rule.json --id rule-001
dustkeeper rule validate --file rule.json            # 只校验，不入库

# 2) 冻结一份不落库的预览，拿到 review 摘要
dustkeeper rule preview --id rule-001 --revision 1

# 3) 用摘要激活这个版本（旧版本授权同时作废）
dustkeeper rule activate --id rule-001 --revision 1 --digest sha256:...

# 4) 单独授予后台能力
dustkeeper rule authorize --id rule-001 --revision 1 --auto-quarantine
dustkeeper rule authorize --id rule-001 --revision 1 --auto-quarantine --digest sha256:...
```

第 4 步不带 `--digest` 时**只输出提案**，里面写明能力、额度、保留期，并明确标注
`retroactive: false` 与 "objects already isolated: NOT covered"。把提案里的 `approval_digest` 原样传回去
才真正写库。这个"两步"是为了让"批准了什么"有据可查，而不是一条命令改掉权限。

额度语义：`--max-files` / `--max-bytes` 取 `min(配置默认, grant 批准值)`。**`0` 表示禁止**，不是无限。

撤销与停用：

```sh
dustkeeper rule revoke  --id rule-001 --revision 1 --auto-quarantine
dustkeeper rule disable --id rule-001 --revision 1
```

`revoke` 只撤一种能力；`disable` 停用整个版本。两者都不移动文件、也不恢复已隔离对象。

---

## 7. 定时任务

`schedule render` **只生成文件，不执行 `systemctl`、`launchctl`、`sudo`**。安装由你自己做，这样"什么时候
获得了什么权限"不隐藏在某条命令里。

最省事的方式是用仓库里的脚本，它在**目标机器上**渲染再安装：

```sh
./packaging/install.sh --dry-run        # 先看它要做什么
./packaging/install.sh                  # 装好并启用
```

不要在一台机器上渲染、复制到另一台：模板里嵌着二进制路径与状态目录，换机器就指错了。手动操作等价于：

```sh
dustkeeper schedule render --platform linux  --output "$TMP" --hour 3 --minute 30
dustkeeper schedule render --platform darwin --output "$TMP" --hour 3 --minute 30
```

生成的 `ExecStart` 是**当前二进制的真实绝对路径**（已解析符号链接）。所以：先装好二进制到最终位置，
再用那个位置运行 `schedule render`；换了路径要重新生成。

### 7.1 systemd（用户级 timer，推荐）

`install.sh` 做的就是这几步，手动等价：

```sh
mkdir -p ~/.config/systemd/user
install -m 0644 "$TMP"/dustkeeper.service "$TMP"/dustkeeper.timer ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now dustkeeper.timer
systemctl --user list-timers dustkeeper.timer
```

模板要点：`Type=oneshot`、`Nice=10`、`IOSchedulingClass=idle`、`UMask=0077`、`NoNewPrivileges=true`。
`Persistent=true` 表示"错过就补一次"，`RandomizedDelaySec=10m` 避免和其它定时任务撞在同一秒。

**关于 linger**：用户级 timer 默认只在用户登录时运行。要让它在无人登录时也跑：

```sh
loginctl enable-linger "$USER"
```

代价是：该用户的进程在登出后继续存活。如果你不希望这样，就不要开 linger，改用系统级 unit
（`/etc/systemd/system/`）并显式设 `User=`——此时要确认该用户对 scope 有读写权限，且**不要**用 root。

### 7.2 launchd（macOS）

`install.sh` 做的就是这几步，手动等价：

```sh
install -m 0644 "$TMP"/dustkeeper.plist ~/Library/LaunchAgents/dustkeeper.plist
launchctl bootstrap gui/"$(id -u)" ~/Library/LaunchAgents/dustkeeper.plist
launchctl print gui/"$(id -u)/dustkeeper" | head -30
```

卸载：

```sh
launchctl bootout gui/"$(id -u)" ~/Library/LaunchAgents/dustkeeper.plist
```

**后台权限与终端不同。** 通过 launchd 启动时：

- 进程没有 TTY，`$HOME` 一般相同，但完整磁盘访问、可移动卷、网络卷的权限**必须单独授予**该可执行文件，
  而不是授予终端。第一次运行 `doctor` 就会暴露这个问题（`ACCESS_DENIED`）。
- 若 scope 在外部卷上，请在"系统设置 → 隐私与安全性 → 完全磁盘访问权限"里加入二进制本身。
- plist 里写了 `StandardOutPath` / `StandardErrorPath`，两者都指向状态目录下的 `run.log`。
- 模板刻意**没有** `KeepAlive`：这是一次性任务，不是常驻进程。

---

## 8. 日常运行与观察

```sh
dustkeeper run --dry-run     # 只对账 + 报告，不扫描不执行
dustkeeper run               # 对账 → 到期清空 → 扫描 → 已授权隔离
dustkeeper status
dustkeeper events --limit 50
dustkeeper events --item <item_id>
```

`run` 的顺序是有意的：先把上次没结束的事说清楚，再考虑新工作。任何一步遇到 `held` / `unknown` /
时钟异常 / 预算耗尽，都会在本轮停在这里，不会带着不确定继续下一个文件。

退出码：

| 码 | 含义 | 定时任务里怎么处理 |
|---|---|---|
| 0 | 完成（含"无候选"） | 正常 |
| 2 | 参数或 DSL 不合法 | 配置写错了，人工修 |
| 3 | 已有实例在跑 | 正常，下一轮再来 |
| 4 | 计划过期 / 身份变化 | 正常（世界变了），下一轮重新计划 |
| 5 | 能力或权限不支持 | 人工处理 |
| 6 | 需对账或安全暂停 | **人工处理**，见第 9、10 节 |
| 7 | 存储失败 | 检查磁盘，下一轮重试 |
| 8 | 模型不可用 | 只影响 `rule draft`，已激活规则不受影响 |
| 9 | 计划只完成一部分 | 看 `recover inspect` |
| 130 | 被信号中断 | 正常，下一轮会先对账 |

用 `--json` 接监控时，`retryable` 字段直接告诉你"重试是否有意义"，不用解析 prose。

---

## 9. 出问题了：恢复与对账

### 9.1 用户主动恢复一个被隔离的文件

```sh
dustkeeper quarantine list --scope exports
dustkeeper quarantine restore --object <item_id>
dustkeeper quarantine restore --object <item_id> --destination-relative sub/restored.zip
```

三种结果：

- 原路径空闲 → 移动回去，并**自动为该路径加保护**（`source=restore`），所以下一次扫描不会立刻又把它选上。
  这不是 bug，是"刚救回来的东西不该当天再被删"。
- 原路径已被新文件占用 → `RESTORE_CONFLICT`（exit 6），**一个字节都没动**。用 `--destination-relative`
  换个位置，或先自己处理掉那个新文件。
- 隔离区里的对象身份对不上 → `HELD`。不要手动 `mv`，先看 `events`。

### 9.2 程序中断之后的收尾

```sh
dustkeeper recover inspect      # 只读，列出 pending 操作
dustkeeper recover reconcile    # 按矩阵补完或解释
```

`reconcile` 只会做两件事：**补完已经发生但没记账的移动**，或者**关闭没有发生的尝试**。它不会重放旧计划，
也不会把保留期倒推到你无法证明的时刻——补完的隔离对象，保留期从对账那一刻重新起算。

### 9.3 hold / unknown 码对照表

`status` 为 `held` 或 `unknown` 时，`events` 里会有对应的 code。按码处置：

| code | 含义 | 人工步骤 |
|---|---|---|
| `OBJECT_MISSING_BOTH_PLACES` | 源和隔离区都没有这个对象 | 从备份确认；确认丢失后关闭该 item |
| `OBJECT_VANISHED` | 移动过程中对象消失 | 检查是否有其他工具在同时清理该目录 |
| `SOURCE_REPLACED` | 源路径被换成了别的对象 | 计划失效，重新扫描；不要试图"修好" |
| `UNEXPECTED_VAULT_OBJECT` | 隔离区里的对象不是计划里那个 | **不要删**；检查 `.dustkeeper/objects` 是否被外部改动 |
| `DUPLICATE_LINK_STATE` | 同一对象同时出现在源和隔离区 | 磁盘快照/回滚导致的典型现象；确认后再手工择一 |
| `CAPTURE_UNREADABLE` / `CAPTURE_NOT_REGULAR` | 打开后拿不到可用的身份或类型 | 检查权限；不是普通文件（socket、fifo）需人工处理 |
| `CONTENT_CHANGED_AFTER_MOVE` | 移动后重新哈希与 manifest 不一致 | 有进程在写入；停掉写入方后重新计划 |
| `POST_VERIFY_READ_FAILED` | 移动后读不回来 | 检查磁盘错误（`dmesg` / 磁盘工具） |
| `DURABILITY_FAILED` | fsync 失败 | 磁盘/文件系统问题；这是最需要立刻处理的一类 |
| `RETENTION_OVERFLOW` | 保留期计算溢出 | 规则里的 `retention_seconds` 不合理 |
| `MISCAPTURE` | 实际移动的对象不是计划里的对象 | 程序已尝试补偿恢复；确认结果后再决定 |
| `RESTORE_CONFLICT` | 恢复时两边都存在 | 见 9.1 |
| `RESTORE_UNEXPLAINED` | 恢复后的状态不符合矩阵 | 手工核对目标路径与隔离区 |
| `SCOPE_UNVERIFIABLE` | scope 根身份复核失败 | 重新注册该 scope |
| `UNKNOWN_ACTION` | pending 操作没有记录动作 | 数据库被外部改动过；从备份恢复 |
| `COMPLETION_VERIFY_FAILED` | 补完时校验失败（附具体原因） | 按附带的原因处理 |
| `SYNC_FAILED` / `COMPLETE_FAILED` / `ABORT_FAILED` | 记账本身失败 | 检查磁盘与数据库可写性 |

**通用原则：`held` 的对象永远不会因为"时间到了"变成可清空对象。** 停下来就是停下来，直到有人处理。

---

## 10. 暂停与解除

```sh
dustkeeper pause      # 写全局暂停标记，返回 PAUSE_REQUESTED (exit 6)
dustkeeper resume     # 清除标记
```

- `pause` 只影响**新的** intent。已经在处理中的文件会走到一个安全边界（当前的 rename + 记账完成）再停。
- `pause` 不改任何 scope 的状态。scope 级别的 `scope pause` 是另一件事：那会阻止该 scope 产生新计划。
- 被 `held` 的 scope 不能靠 `resume` 恢复，必须重新注册——原因是根身份已经不可信，换个标记没有意义。

---

## 11. 备份

要备份两样东西，它们必须**一起**备份：

1. `<state>/state.db`（含 `-journal` 文件，如果存在）
2. 每个 `managed_disposable` scope 下的 `.dustkeeper/`（`owner.json`、`objects/`、`manifests/`）

只备份数据库会得到一堆指向不存在对象的记录；只备份隔离区则失去身份与保留期信息。

```sh
# 先确保没有任务在跑
dustkeeper pause
dustkeeper status                 # 确认没有 pending_operations
sqlite3 "$STATE/state.db" ".backup '/backup/state.db'"
tar czf /backup/vault-exports.tgz -C /srv/app/disposable-exports .dustkeeper
```

`status` 里只要还有 pending 操作，就先跑 `recover reconcile` 再备份。备份一个"半途"状态，恢复时会更难解释。

---

## 12. 卸载

顺序很重要，**先停调度，再停程序，最后才动目录**：

```sh
# 1) 停掉定时任务
systemctl --user disable --now dustkeeper.timer          # 或 launchctl bootout ...
loginctl disable-linger "$USER"                          # 如果开过

# 2) 让正在跑的实例走到安全边界
dustkeeper pause
dustkeeper status                                        # 等 pending_operations 归零

# 3) 处理仍未恢复的对象
dustkeeper recover inspect
dustkeeper quarantine list --scope exports               # 逐个 restore 回去

# 4) 确认隔离区已空
ls -la /srv/app/disposable-exports/.dustkeeper/objects   # 应为空

# 5) 移除程序与状态
rm /usr/local/bin/dustkeeper
rm -rf "$STATE"                                          # state.db、日志、锁
rm -rf /srv/app/disposable-exports/.dustkeeper           # 仅在第 4 步确认为空之后
```

第 5 步最后一条要特别小心：**隔离区里如果还有文件，`rm -rf .dustkeeper` 就是不可恢复的永久删除**，而且绕过了
`purge` 的全部检查。要么先恢复，要么明确接受这个后果。

`protections` 表里的路径保护不影响其他工具，可以不清理。

---

## 13. 故障速查

| 现象 | 先看这里 |
|---|---|
| `run` 一直隔离 0 个 | `rule preview` 是否还有候选；`rule authorize` 是否只打印了提案没提交摘要 |
| 刚恢复的文件又被选中 | 预期行为：恢复后自动加路径保护；若仍被选中，看 `protect list` 是否写失败 |
| 计划刚生成就 `PLAN_STALE` | 有进程在写该目录（父目录身份或 mtime 变了）；`--writers-stopped` 之后重建计划 |
| `plan apply` 报 `INVALID_ARGUMENT` | `manual_duplicate` 计划必须显式 `--writers-stopped` |
| `purge apply` 报 `DIGEST_MISMATCH` | 计划内容在生成后变了；`purge show` 会说明重算摘要是否与存档一致 |
| 隔离区一直增长 | `auto_purge` 没开，或保留期还没到；`purge plan` 会列出到期对象 |
| `INSTANCE_BUSY` 反复出现 | 有残留实例或另一个定时任务；`ps` 查一下，锁文件在 `<state>/instance.lock` |
| macOS 上 `EXC_BAD_ACCESS` / `EBADF` | 权限授予对象错了（授予终端而非二进制）；或磁盘异常 |
| 时间相关测试失败 | 检查时区与 `TZ`；程序内部全部用 UTC 纳秒 |
| `ACCESS_DENIED` 但 `ls` 正常 | 程序用 fd 相对路径 + `O_NOFOLLOW`，不跟符号链接；检查路径中是否有链接段 |
