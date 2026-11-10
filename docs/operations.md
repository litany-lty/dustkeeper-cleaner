> 本地开发分支提示：新增 Q/P 双重审核门禁，原离线安装流程不足以启用本分支。正式版本和制品保持原样；新分支的配置、证据与未完成发布条件见 [安全审核说明](safe-review.md)。

# dustkeeper 运维手册

面向要在一台真实机器上把它装起来、跑起来、并在出问题时知道该看哪里的人。

装之前先读这一句：**这个程序的第一版故意把"能自动做的事"压到很小**。装上之后它会一直在做"扫描 + 报告"，
除非你逐项打开规则激活、自动隔离、自动清空这三个开关。如果你只想要一个只读的观察者，装完不做第 5、6 步
就可以，它不会移动任何文件。

---

## 1. 前置条件

| 项目 | 要求 |
|---|---|
| 平台 | 正式发布的制品只有两个：`linux-amd64`、`darwin-arm64`。其它系统或架构没有制品，也没有发布验证；自行从源码构建属于自建，不是受支持的发布 |
| 文件系统 | 与被清理目录同卷的普通本地文件系统。需要支持排他 rename（Linux `renameat2`、macOS `renameatx_np`）；网络卷、FUSE、部分容器 overlay 不支持 |
| 内核 | Linux ≥ 5.6（`openat2`）；以下版本会在打开 scope 时失败，而不是降级 |
| 权限 | 以**要清理的文件的所有者**身份运行。不要用 root 去清理用户目录 |
| 进程列表 | 自动隔离与自动清空都需要一份完整的进程列表（`lsof`，macOS 自带，Linux 需安装）。程序按 `/usr/sbin`、`/usr/bin`、`/sbin`、`/bin` 顺序查找它，找不到时才用 `PATH`。**拿不到列表时程序不会移动任何文件**，结果是 `KEPT / facts_missing`，原因会写在结果页与 `--json` 的 `message` 里 |
| 磁盘 | 状态库与隔离区都占用空间。隔离区里放的是原文件本身，按"最多保留 N 天 × 每日隔离量"预留 |

一期不支持：跨卷移动、跟随符号链接、清理系统目录或应用包内部。

---

## 2. 安装

发布目录里是四个文件，来自同一次构建，必须一起下载：`dustkeeper-1.0.0-phase1-<target>`
（二进制本身，没有压缩包）、`SHA256SUMS`、`release-manifest.txt`、`build-facts.txt`。

```sh
# 在当前目录（四个文件都在这里）
shasum -a 256 -c SHA256SUMS
grep -E '^(version|commit|target|artifact|sha256)=' release-manifest.txt
install -m 0755 dustkeeper-1.0.0-phase1-linux-amd64 /usr/local/bin/dustkeeper
dustkeeper version
```

第一步用 `SHA256SUMS` 核对二进制字节，第二步核对这份清单说的是哪个 commit、哪个 target：
清单里的 `commit` 是二进制的内嵌值，不是文档写的值。macOS 上把最后两行的文件名换成
`dustkeeper-1.0.0-phase1-darwin-arm64`；写 `/usr/local/bin` 一般要 `sudo`，你也可以装到自己的
`~/.local/bin`（记得在 `schedule render` 之前装好，模板记的是那个位置的绝对路径）。

`version` 的输出里带 `commit`，应当与清单里的 `commit` 一致——不一致说明装错了文件。

macOS 上 `build/build.sh` 产出的二进制已做 ad-hoc 签名（`codesign --force --sign -`）。如果你自己重新编译，
也用同样的方式签一下，否则每次执行都会被系统重新评估，首次运行会明显变慢。通过浏览器下载的文件可能带
`com.apple.quarantine`，首次执行前用 `xattr -d com.apple.quarantine <binary>` 去掉，或改用命令行下载。

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

日志目录里是 `dustkeeper.jsonl`（当前文件）与 `dustkeeper.jsonl.1..3`（轮转后的历史）。一个大小上限
2 MiB、最多保留 3 个历史文件，单条记录的字符串字段超过 4 KiB 会被截断并标记 `...[truncated]`；文件权限
0600。写入发生在**持实例锁的命令**内（`begin` 阶段一条，`result` 阶段一条，`run`/`reconcile` 另加
每 scope、每操作各一条），`help`/`version` 这类只读命令不写日志。

每条记录是一行 JSON，可直接 `jq`：

```sh
# 昨天的失败，以及程序建议下一步做什么
jq -c 'select(.phase=="result" and .exit!=0) | {command,code,message,next_steps}' \
  "$STATE/logs/dustkeeper.jsonl"

# 某次 run 的结论与各 scope 的停止原因
jq -c 'select(.phase=="run" or .phase=="scope") | {phase,run_id,scope_id,control,message}' \
  "$STATE/logs/dustkeeper.jsonl"
```

日志里不含文件内容、清单正文、模型提示词与密钥：它记录的是"发生了什么"，不是"数据是什么"。

`DUSTKEEPER_CONFIG_DIR` / `DUSTKEEPER_STATE_DIR` 可以覆盖以上路径。**同一台机器上跑多个实例时**必须分别
设置——但请注意 owner marker 仍然会阻止两个状态库管理同一棵目录树。

注册时还会在 `$HOME` 下创建 `.dustkeeper-registry.lock`（注册锁）。它故意放在 home 而不是状态目录：
换状态目录不该造出两把互不相干的锁去抢同一棵树。

### 3.1 升级已有实例：先确认手里是哪一版

状态库带 `PRAGMA user_version`。**本工作区这一版是 schema 6**（`migrations/006.sql` 增加
`review_dispatch_resolution` 人工核查表；`005.sql` 保持不变）。已知的旧库（schema **3、4 或 5**）**只能由显式
`init` 升到 6**；`status` / `run` / 其它普通命令一律通过 `OpenExisting` 拒绝版本不一致的库，不改一个字节。
也就是说，"换上新二进制再跑一次 `run` 就自动升级"不成立，但"备份之后跑一次 `init`"会升级，并且会在
`init --json` 的 `schema_upgrade` 里如实写出 `from` / `to` / `upgraded`。本机托管实例当前是 **schema 3**，
若本轮代码被正式安装，升级结果应写作 **3 → 6**，不是"仍为 5"。

`init` 的升级不是无条件的：`internal/store/upgrade.go` 会先核对文件是普通文件（不是 symlink）、文件头与
`PRAGMA user_version` 一致、`meta/instance_id` 存在、核心表齐全、sidecar 为空；`headerVersion` 与
`user_version` 不一致、版本比 3 更旧或比 6 更新、以及库里没有实例身份时都拒绝并保持版本不变。逐 step 在
事务里执行，同时写 `meta('schema_version')`；迁移结束后再核对 `PRAGMA user_version` 已是 6 且
`review_dispatch_resolution` 确实存在（`proveUpgradedSchema`），否则不报升级成功。迁移前的
`proveOurDatabase` 故意只要求 3/4/5 都有的核心表：若在这里要求 schema-6 新表，所有 3/4/5 → 6 升级都会被拒绝。
这一步是交接计划
[dustkeeper-next-steps-20261106.md](tasks/dustkeeper-next-steps-20261106.md) §5.3 要求的显式升级边界。

无论哪一版，升级前都遵守：

- 非空 `-journal` / `-wal` / `-shm` 先保留现场，用**旧二进制**打开并正常关闭；不删 sidecar、不改
  版本号、不重建库绕过核对。系统自带 `sqlite3` 可能不认识本项目的 STRICT 表，别把它写成通用步骤。
- 升级后核对 `instance_id`、scope、规则授权、保护、隔离对象、pending 与 `review unknown list` 的未解决
  行是否都还在；`init` 退出 0 只说明它自己跑完了，不说明历史状态被保住。
- 升级不调用模型、不移动对象、不自动启用调度。先核对完，再单独决定是否 `resume`——`resume` 只清全局
  暂停标记，不恢复 scope 状态，也不补发授权。
- **状态库副本不是沙箱**（见 §11）。库里记着原 scope 的绝对路径；对副本跑 `run` / `restore` 仍会动真实
  文件。演练要用独立测试目录与独立实例。

### 3.2 目录身份变化：`scope rebind`，不是 `resume`

Darwin 的 `dev-<st_dev>` 是**运行期设备号**，不是已核实的稳定卷 UUID；重启后 APFS 数据卷可能重新编号。
跨启动出现 `dev-16777229 → dev-16777231` 时，持久证据（inode、创建时间、owner、文件系统）以及
`.dustkeeper/` 里的 nonce、anchor tag、6 份 manifest 可能全部未变，但旧程序会把这个变化记成
`SCOPE_CHANGED` 并把 scope 置为 `held`。**`scope resume --accept-stale-hold` 不能修这个**：它只改状态列，
不重建根身份、vault 绑定、运行期挂载记录或隔离对象的身份字段，下一次打开仍会拒绝。直接删除 scope、
清空隔离区或另建空实例都是不允许的绕过。

本版新增两步式、保留历史的恢复入口：

```sh
# 1) 只读预览：证明旧/新证据、影响范围、隔离对象恢复链，并输出 digest；不改任何状态
dustkeeper scope rebind --id downloads --json

# 2) 只在调度已停止、一致备份存在、全局暂停、无在途操作、具体动作已获批准后确认
dustkeeper scope rebind --id downloads --digest <预览给的 digest> --confirm \
  --backup /absolute/private/backup-dir --schedule-stopped --note "已核对重启后的卷与隔离对象"
```

预览逐项列出：scope / 实例 / 原路径、当前与新 generation、旧新身份差异、owner / vault 卷标签、
隔离对象数量与逐个「对象仍存在、身份可对应、manifest 存在、恢复链有效」的结论、会失效的计划与授权、
保护数、pending 操作数、全局暂停状态，以及不可自动处置的对象。确认时**重新验证同一套事实**；
身份又变了、owner/vault 不匹配、存在未完成文件操作或 digest 过期，一律拒绝且不开放清理。

确认后在一个事务里完成：按结构化字段逐个改写根身份、vault 身份摘要、运行期挂载记录与隔离对象的
身份 JSON（**不是**批量替换数据库里的 `dev-*` 字符串），generation 与保护版本各 +1，active grant
收窄为 `needs_reapproval`，并写一条不可变审计事件保存全部旧证据与映射来源。scope ID、全部历史记录、
原隔离内容、manifest、保护与审核历史保持原样；旧计划不删除，但因 generation / identity 绑定不再可直接执行，
需要重新预览并确认适用规则。scope 落到 **`paused`**（不是 `active`），全局暂停标记不动，不启动模型、扫描、
恢复、清理或调度。升到 schema 6 之后不能只换回旧二进制；回退边界见 §11。

> 本版只把**跨启动**的 legacy `dev-*` 重编号当成运行期标签变化处理；同一次启动内 device / mount 变化仍然
> 视为真实挂载替换并被拒绝。非 `dev-*` 形状的稳定卷标识始终严格比较。跨启动行为尚未在真实设备上做长期
> 验证，不能写成"已彻底解决"。真实实例的 6 个隔离对象在本轮整改中**未做重绑定**，仍保持 `held`。

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
| `reference` | 否 | 永不移动，也不进入 `run`。这是早期查重功能留下的"正本"目录类型，现在的版本不再产生查重计划；保留它是为了让已经登记的对象继续被认出来，不要为新用途注册这个类型 |

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

`--acknowledge-clock` 是一个事务，不是一个时间戳：它清除时钟闸门，并把**保留窗口无法证明的对象**按现在
重新给足一个完整窗口（`doctor` 的输出会写明有几个对象被重新计时），然后记下这次确认。它只会把窗口往后推，
不会缩短任何对象的保留期，也不会让谁提前变成可清空。已经有确定起始时间的对象不受影响。

这也是它和"什么都不做"的区别：闸门在时清空是停住的，确认之后清空会恢复，但恢复的第一个动作是重新给窗口，
而不是立刻删。不确定的时间不能被一次确认抹平成"早就到期了"。

**`doctor` 不检查模型链路。** 它查目录、数据库、文件系统能力、暂停标记、待对账、时钟与各 scope 的根身份；
不读 `review` 配置，不检查凭据文件、CA 或端点连通性。`doctor` 全绿只说明这台机器"能安全地移动文件"，
不说明模型能用——目标机上"环境 + 模型"这两半要分别验证。要在一个目录上做只读的模型检查，用
`debug --root <dir> --execute`（advisory 问答，不移动任何文件）；否则就只能靠一次真实 `run` 来证明。

---

## 6. 规则：从草稿到授权

规则预览要有候选可看，所以先扫一遍：

```sh
dustkeeper scan --scope exports     # 只读；把观察到的东西记进观察表，不移动任何文件
```

扫描不完整的 scope（权限缺失、有目录读不了）不会进入新的自动隔离，这条在执行时还会再查一次。

然后是规则的导入与授权。日常只有两条命令：`rule enable` 打印提案，`rule enable --digest` 确认。

```sh
# 1) 导入（手写 JSON 规则；导入本身不构成任何授权）
dustkeeper rule import --file rule.json --id rule-001

# 2) 提案：会激活哪个修订、绑定哪次 review、授予哪种能力、额度与保留期
dustkeeper rule enable --id rule-001 --revision 1

# 3) 确认：把上一步给出的 enable digest 原样传回去
dustkeeper rule enable --id rule-001 --revision 1 --digest sha256:...
```

`rule enable` 不带 `--digest` 时**只输出提案**，里面写明能力、额度、保留期，并明确标注
`retroactive: false` 与 "objects already isolated: NOT covered"。把提案里的 `approval_digest` 原样传回去
才真正写库，而且"激活 + review 绑定 + 选定能力"在同一个事务里，要么全成要么全不成，不会留下半激活的规则。
`--mode cleanup` 才是"隔离后到期清空"，`--mode quarantine`（默认）只隔离。

底层入口仍然保留，需要分开做或逐项核对时用：

```sh
dustkeeper rule validate --file rule.json            # 只校验，不入库
dustkeeper rule preview --id rule-001 --revision 1   # 冻结一份不落库的预览，返回 review 摘要
dustkeeper rule activate --id rule-001 --revision 1 --digest sha256:...   # 旧版本授权同时作废
dustkeeper rule authorize --id rule-001 --revision 1 --auto-quarantine    # 不带 --digest 时只打印提案
```

分开做时，两次确认之间规则或目录有任何变化，都要重新确认——摘要算的就是当时那份内容。

`rule draft`（让模型写规则草稿）是**未验收**的入口，默认没有可用端点时直接 `MODEL_UNAVAILABLE`（exit 8），
它写出的草稿不构成任何授权。正式上手路径用上面手写的离线 JSON 规则。

额度语义：`--max-files` / `--max-bytes` 取 `min(配置默认, grant 批准值)`。**`0` 表示禁止**，不是无限。

撤销与停用：

```sh
dustkeeper rule revoke  --id rule-001 --revision 1 --auto-quarantine
dustkeeper rule disable --id rule-001 --revision 1
```

`revoke` 只撤一种能力；`disable` 停用整个版本。两者都不移动文件、也不恢复已隔离对象。

### 6.1 两种模式：隔离与清空

| 模式 | 实际作用 | 什么时候用 |
|---|---|---|
| `--mode quarantine`（默认） | 只把匹配文件移到 `.dustkeeper/objects`，随时可以用 `quarantine restore` 拿回来；保留期到了也不会自动清空 | 第一次试运行、目录用途还在确认 |
| `--mode cleanup` | 在隔离之外再批准"保留期结束后自动永久清空"；每轮仍受独立额度、授权与本地检查约束 | 目录用途明确、规则已经跑过几轮确认无误 |

**隔离是同卷 rename，通常不释放文件的数据块。** 真正的空间回收发生在永久删除之后；APFS 快照、仍被进程
打开的文件也会让系统显示的可用空间滞后。所以不要把"隔离了多少字节"当成"已经释放了多少空间"。

后来把规则改成 `--mode cleanup`，不会让之前只隔离的对象追溯获得自动清空权限——它们仍然要走人工计划或恢复。
永久清空之后，Dustkeeper 不能把它撤销。

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
dustkeeper run --dry-run     # 只读：列出 pending 操作，不对账、不扫描、不移动
dustkeeper run               # 对账 → 到期清空 → 扫描 → 已授权隔离
dustkeeper status
dustkeeper events --limit 50
dustkeeper events --item <item_id>
```

`--dry-run` 回答的是"现在有什么没结完"，不是"下一轮会删什么"：它连对账都不做，列出来的 pending 仍然要由
`recover reconcile` 处理。垃圾候选的预览不在这个命令里——先 `scan` 把目录扫进观察表，再用 `rule preview`
（或 `rule enable` 不带 `--digest` 时的提案）看这条规则会选中哪些文件。

**`run` 不支持 `--scope`。** 它处理的是整个实例当前可能触达的所有 scope，不是某一个目录。首跑之前必须核对
整个实例登记了哪些 scope、pending / 到期清空队列里有什么、以及确认过的清理范围与授权覆盖了哪些目录；
不要以为加一个 `--scope` 就能把首跑限制在一个小目录里。想缩小范围，要在**注册与授权**层面做到，而不是在
`run` 命令上加参数。

`run` 的顺序是有意的：先把上次没结束的事说清楚，再考虑新工作。任何一步遇到 `held` / `unknown` /
时钟异常 / 预算耗尽，都会在本轮停在这里，不会带着不确定继续下一个文件。

退出码：

| 码 | 含义 | 定时任务里怎么处理 |
|---|---|---|
| 0 | 完成（含"无候选"） | 正常 |
| 2 | 参数或 DSL 不合法 | 配置写错了，人工修 |
| 3 | 已有实例在跑 | 正常，下一轮再来 |
| 4 | 计划过期 / 身份变化 / 授权失配（`GRANT_REQUIRED`） | 正常（世界变了），下一轮重新计划；授权失配见 9.1.1 |
| 5 | 能力或权限不支持 | 人工处理 |
| 6 | 需对账或安全暂停 | **人工处理**，见第 9、10 节 |
| 7 | 存储失败 | 检查磁盘，下一轮重试 |
| 8 | 模型不可用 | 只影响 `rule draft`，已激活规则不受影响 |
| 9 | 计划只完成一部分（含"批次预算用完、但前面的对象已经做完"） | 看 `recover inspect`；未开始的候选不算失败，下一轮在总预算内继续 |
| 130 | 被信号中断 | 正常，下一轮会先对账 |

用 `--json` 接监控时，`retryable` 字段直接告诉你"重试是否有意义"，不用解析 prose。

### 8.1 三结果与结果页

一轮清理只产生三种结果：**已删除**、**待删除（可恢复）**、**保留**。每个文件只能落在其中一个里，
结果页也只有这三节。

```sh
dustkeeper cleanup setup --scopes downloads --categories archive_unknown_contents --observation-days 14
# 只打印"将被批准的五件事"+ digest；确认时把 digest 原样传回去（两步，先看后批）
dustkeeper cleanup setup --digest <digest> --scopes downloads --categories archive_unknown_contents --observation-days 14
dustkeeper run              # 结尾一行：本次删除/待删除/保留各几个 + 结果页路径
dustkeeper cleanup report   # 原样打印最近一页；--json 输出页面数据
```

结果页默认在 `<state>/reports/latest.md`（同目录 `latest.json`，以及每一轮的 `result-<run_id>.md`），
可用 `cleanup setup --result-dir` 改到别处。**没有确认策略时**页面落在 `<state>/reports`，因为
"没确认"就是"没批准任何目录"，`setup` 预览里给的目录还不算数——`run` 打印的永远是它实际写入的路径。

要记住的五条：

1. **观察期取两者较大值。** 新对象 = `max(规则保留期, 策略观察天数)`。策略只能抬高下限，不能缩短已经
   记录过保留期的对象；旧对象的保留期字段一个字都不改。
2. **直删需要四件事同时成立。** 类别在确认过的直删列表里、本地检查无阻碍、双方模型明确同意、授权有效。
   少一件就只能是"待删除"。类别列表默认是空的。
3. **通知没送达就不清空。** 永久清空前会发一次系统通知；通知失败或无人值守时如实返回失败，这一轮不清空，
   对象继续留在观察区（可恢复）。结果页照常生成，`run` 会打印 `the user was not told: …`。
   **明确确认 `--notify none` 是另一回事**：那不是"没送达"，是你自己关掉的出口。此时发送器本身不再被执行
   （不会调用 `osascript`），动作照常执行，结果页照旧写，`run` 打印 `notifications are off: …`，
   事件里的通道是 `none`。服务器上人工查报告就用这种模式；要主动送达，就把结果页摘要接到你自己的
   运维通道，这里没有第二个通知平台。
4. **保留会写进页面。** "模型说保留"和"模型服务失败"是两个不同的原因码：前者是结论，后者是没结论。
   两者都不产生 pending，也不会让这个范围停摆。
5. **数字只有一套。** `run` 的摘要行和结果页读的是同一份汇总（`report.Page`），不是两处各算一次。

**放行闸门是可校准的，但默认为严。** JEV 必须同时满足"置信度 ≥ 90%"和"允许概率 ≥ 90%"才会给出 allow，
而 allow 同时是"直接删除"和"进入观察区"的前置条件。在真实端点上实测 30 条返回（13 条 `allow_review`），
置信度最高 0.79、允许概率最高 0.86——默认值下一条都过不去，所以这台机器上它**既不删也不移**。
想让删除这条路可达，就显式调低（单位是整数百分比，配置格式不接受小数）：

```json
{"review": {"jev_min_confidence_pct": 70, "jev_min_allow_probability_pct": 80}}
```

调低多少是风险决定，不是配置问题：同一组样本在 70/80 下四次里只有一次两方都同意。
**调低就是扩大放行范围**——代码把两个值直接与最低门槛比较，原来被拒绝的答案会变成允许，
所以不要把它当"适配模型量表"的中性操作，也不要拿调低后的结果去证明准确率。`timeout_seconds`
（5..120，默认 30）控制单次模型请求的等待；实测 20s 会让 MiniMax 偶尔超时。超时不会被等下去：该候选记为
"请求已发出、结果未知"（`review_unknown`，文件原位不动、费用未知），而不是悄悄当成"没有模型意见"。
任务与批次的时间额度见 8.2：`runtime.run_soft_seconds` 默认 300，且必须容得下 `2 × timeout_seconds +
reserve_seconds`，写小了会在加载配置时被拒绝。

**自签或内网端点：`trust_ca_file` 必须是一条全程没有符号链接的真实路径。** 凭据与 CA 都是逐段
`O_NOFOLLOW` 打开的，路径里只要有一段是链接就直接 `MODEL_UNAVAILABLE`，而那个错误信息不会指向路径。
macOS 上 `/tmp`、`/var` 本身就是链接，Linux 上不少发行版的 `/home` 也是；先 `realpath` 再用解析后的路径
写进配置。

**外网 JEV 需要目标机自己有一条可用的出口。** 直连被地域策略挡住时（HTTP 451），JEV 那一路先失败，
本轮候选一律按"没有模型结论"保留——调不通不会变成放行；其它已经发生的动作仍以结果页为准。
出口填 `review.jev_proxy_url`（只接受 `http://host:port`，
不带用户名密码、query 或路径，程序不做直连回退）；MiniMax 不走这条代理。程序不会凭空提供代理出口，
服务器上没有可用的 HTTP CONNECT 服务就得先装一个。

页面里的"已删除"只认**已完成并确认**的动作：候选、模型意见、intent、debug 模拟都不计入。超过 200 条时
显示总量与"还有 N 条未显示"。`cleanup report` 是只读命令：它读文件，不移动、不删除、也不写审计。

仓库里另有一个独立工具 `cmd/cleanup-report`，那是**只读目录盘点**（看使用痕迹、已知引用、空间上限），
不读状态库、不生成许可，和这里的 `cleanup report` 不是一回事。

### 8.2 审核的钱花在哪里：三层预算

审核一次文件要走两次外部请求（JEV，然后 MiniMax）。这两次请求可能被三种互相独立的额度截断，**每一层
都有自己的上限，互不挪用**：

| 层 | 管什么 | 参数（括号内是默认值） | 用完之后 |
|---|---|---|---|
| 整次任务 | 一个 `run` 里所有 scope 共用的时间与候选数 | `runtime.run_soft_seconds`（300，5..600）、`review.max_candidates`（1..50） | 剩余 scope 不开始，`run` 以 `BUDGET_EXHAUSTED`（exit 9）结束 |
| 一个批次 | 一个 plan（一条规则的一批候选）的候选数、HTTP 尝试次数、秒数 | `review.batch_max_candidates`（5）、`review.batch_max_attempts`（10）、`review.batch_seconds`（180）、`review.reserve_seconds`（10） | 本批剩下的候选标记为**未开始**，不进入下一批 |
| 单次请求 | 一次 HTTP 调用的等待时间 | `review.timeout_seconds`（30，5..120） | 该候选记为请求超时（已发出、结果未知） |

三条必须知道的规则：

1. **提高候选上限不会买来更多时间。** `max_candidates` 只决定"最多看几个"，不决定"有多少秒"。想一次多处理，
   就得同时抬高 `batch_seconds` 与 `run_soft_seconds`，或者把候选拆成多批。
2. **子请求不越过父截止时间。** 每个候选开始前先检查"还能不能装下一条完整的两段请求 + 收尾余量"；装不下就
   干脆**不开始**，而不是发出去再等超时——那是花钱买一个必然会丢的答案。`reserve_seconds` 就是留给收尾的
   那一小段。
3. **配置自相矛盾时拒绝启动，不静默钳制。** `batch_seconds` 必须容得下 `2 × timeout_seconds + reserve_seconds`，
   `run_soft_seconds` 同样必须容得下一条完整路径加收尾；写小了会在加载配置时直接报错（分别提示
   `review.batch_seconds … cannot contain one dual review path plus reserve` 与
   `runtime.run_soft_seconds … cannot contain one dual review path plus reserve`）。宁可启动失败，也不要
   出现"第二段请求永远被丢掉、却记成供应商故障"的结果。
4. **新候选必须预留两个 HTTP 尝试额度。** 一条完整路径可能要问两次，所以在第一个请求发出之前，任务与批次
   都必须还剩下 2 次。`batch_max_attempts: 1` 是合法配置（校验只要求 ≥1），但结果是**一个候选都不会开始**：
   结果页写 `not_started_budget`，不会发出半条路径。想让审核真的开始，把该值调到 ≥2。

### 8.3 四种"没有结论"，以及各自怎么处置

有结论（keep / allow）的文件好办；难办的是**没有拿到结论**的那几种，它们看起来都像"保留"，但下一步完全不同。
结果页的「本轮审核」小节、`run` 结尾那一行、以及可读日志的「审核情况」小节读的是同一份计数：

| reason | 退出码 | 状态 | 含义 | 人工下一步 |
|---|---|---|---|---|
| `not_started_budget` | 9 `BUDGET_EXHAUSTED` | 未开始 | 预算已经用完，**这一轮没有为它发出任何请求**，因此不产生请求费用 | 下一轮在原有授权与总预算内继续；不需要重新审批 |
| `review_unknown` | 8 `MODEL_UNAVAILABLE` | 请求已发出、结果未知 | 请求确实发出去了（`http_attempts ≥ 1`），但没拿到答复；**文件原位未动**，费用未知 | 先核查这次请求，再决定是否再问一次（见下） |
| `awaiting_request_check` | 0 `KEPT` | 未重复请求 | 同一个对象、同一个动作，之前那次请求的结果还没人核查；本轮**故意不再问** | 同上；这是"不重复付费"的具体动作 |
| `review_unavailable` | 8 `MODEL_UNAVAILABLE` | 模型服务失败 | 请求没有取得可用答复（未发出、被拒绝、或返回不可解析） | 修好端点/凭据/额度后正常重试即可 |

`review_unknown` 与 `awaiting_request_check` 背后是同一条记录，落在状态库里（`review_unknown` 表），不是
只写在日志里：

```sh
dustkeeper review unknown list              # 还没核查的请求（--all 连已核查的也列；默认最多 200 行，上限 500）
dustkeeper review unknown resolve \
  --item <item_id> --action quarantine \
  --note "核对了供应商控制台：该请求超时，未计费" --confirm
dustkeeper review dispatch list             # 仍未解决：pending 且没有人工核查结论（输出可复制的 dispatch_id）
dustkeeper review dispatch list --all       # 连 settled / refused 的历史与独立人工结论一起列
dustkeeper review dispatch resolve --dispatch <dispatch_id> \
  --request-status not_sent --billing-status not_charged \
  --note "核对了供应商控制台：该请求未发出、未计费" --confirm
```

`list` 默认只把「pending 且没有人工核查结论」当作未解决；`--all` 把**准备时记录**（state、attempts、
failure、cost_status）与**人工核查后的事实**（request_status / billing_status / note / resolved_ns）分行显示，
不混在一起。原始 `cost_status`、attempts、token 用量不因人工核查被倒改。

请求发出**之前**会先写一条 `review_dispatch` 的 `pending` 准备记录，审核结果与 decision event 在同一事务里
结算为 `settled`；本地在发请求前就拒绝的写成 `refused`（`cost_status=not_sent`），不会留下一个假问题挡住下一轮。
进程在"已发请求、未落结论"之间崩溃留下的 `pending` 会像未知请求一样阻止自动再问——用
`review dispatch list` 找出它，再用 `review dispatch resolve` 按**精确 dispatch_id** 人工收口。

`resolve` 需要 `--confirm`、非空且有长度上限的 `--note`、已核查的 `--request-status`
（`not_sent` / `completed` / `failed`）与 `--billing-status`（`not_charged` / `charged`）。
`unknown` 不能自动放行，`not_sent + charged` 的矛盾组合被拒绝。它只处理指定的未结算 dispatch：
已 `settled` / 本地 `refused` 的请求不能用它重新开放；重复确认不会覆写第一份核查记录；
其他未结算请求、其他动作和 `review_unknown` 记录继续独立生效，命令会明确报告而不是保证"下次一定能问"。
结论落在独立的追加式 `review_dispatch_resolution` 表并写一条独立人工事件（不增加模型审核次数、
HTTP attempts、token 用量或文件动作数）；事务里任一写入失败全部回滚，不能出现"记录没写全但已解锁"。
命令不调用模型、不移动文件、不增加额度，也不代表旧审核允许删除；下次运行仍经过预算、身份、保护、
规则与模型门禁，并产生新的 request / review ID。执行者须自行核对允许再次调用的批准与剩余额度。

`--all` 可以核对某次 `review_id` 是否真的结算过；`review_id` 同时出现在结果页、可读日志、
decision event 和运行日志里，是这四处的关联键。

- `list` 给出的路径、动作、尝试次数、失败类别与费用状态就是判断依据。**这条记录的唯一作用就是阻止下一轮
  自动再花一次钱去问同一个问题**；删掉状态库能达到同样效果，但那等于销毁"付过费、没答复"的事实。
- `resolve` 不是"删掉记录"，而是**写一条核查结论并允许再问一次**；旧的那次请求仍然留在库里。缺 `--confirm`
  或缺 `--note` 都会被拒绝（返回 `NEEDS_INPUT`，exit 2）——没有核查说明的"确认"不是一个记录。
- 重新扫描、改规则、重装二进制都不会清掉这条记录；只有 `resolve` 会。这是有意的：规则变化不能让一笔
  未知费用变成"从未发生"。

### 8.4 保留哪一套：`retention plan`

"只留最新的一套"这句话的真实含义是"只留一套**完整可恢复**的版本集合"，而不是"留最新那个文件名"。目录列表
回答不了这件事（客户端的版本可能比服务器旧，版本号大小也不等于先后），所以用一个**声明式清单**回答：每个
应用登记自己的发布集合、每个集合由哪些组件组成、以及当前有哪些活动引用。

```sh
dustkeeper retention plan --manifest /srv/apps/manifest.json
```

清单必须是绝对路径、普通文件、不超过 1 MiB，`schema` 固定为 1：

```json
{
  "schema": 1,
  "applications": [
    {
      "name": "elidli-server",
      "releases": [
        {"id": "rel-2026-11-01", "published_at": "2026-11-01T10:00:00+08:00", "published": true,
         "complete": true, "active": true,
         "components": [
           {"path": "/srv/apps/elidli/releases/rel-2026-11-01/application.jar", "sha256": "…"},
           {"path": "/srv/apps/elidli/releases/rel-2026-11-01/application.before.jar", "sha256": "…"}
         ]}
      ],
      "references": [{"path": "/srv/apps/elidli/releases/rel-2026-11-01/application.jar",
                      "why": "systemd unit ExecStart", "active": true}],
      "protections": [], "unresolved": []
    }
  ]
}
```

判定顺序（先命中先算）：**活动引用 → 最新合格集合 → 进行中恢复 → 明确保护 → 未解决的不确定**。其余成套
版本进入候选。两条关键行为：

- **不合格的集合整套保留**，理由是 `incomplete_release_kept_for_inspection`：缺一个组件的"最新版"不能当
  最新版用，也不能因为它新就把旧的完整集合删掉。此时 `goal_met` 为 `false`，命令仍以 0 退出——"目标没达成"
  是一个要读的结论，不是要重试的故障。
- 只按清单说话，不按最大版本号、也不按文件数量。客户端与服务器组件版本不同时，仍然按"这一套"整体保留。
- **共享路径的「保留」结论优先。** 同一个路径可以被两个应用声明（例如共用的 jar）。清单不再因为共享而整体
  拒绝：只要任何一个应用判定它要保留，其它应用就不会把它列为清理候选，并且各自在自己的 `keep` 里记下
  `shared_path_kept_by_another_application` 与保留方。`shared_paths` 会列出所有共享路径。没有任何一方保留
  时，候选仍在，但计划会写明"共享归属未澄清"，不会假装这是一个普通候选。`goal_met` 因此会诚实地为 `false`。

它**只出计划，不移动任何文件**：计划里的候选还要走 `rule preview` → `plan create` → 双模型与授权门禁，
和别的候选同一条路。

### 8.5 磁盘诊断、系统日志维护、应用服务变更：三件事分开记账

这三件事经常被一句"清一下服务器"混在一起，混起来就没法解释"空间到底被谁放掉的"：

1. **磁盘诊断**（分区为什么满、谁在写、inode 是否耗尽）是只读分析。`du` / `df` / `lsof` 的结论不能当作
   清理效果：本程序只在已登记的 scope 内处理候选，分区可用空间的变化还包含隔离区占用、日志增长、数据库本身。
   结果页分开写"删除的逻辑大小"和"磁盘可用空间前后"，不把前者冒充后者。
2. **系统或应用日志维护**（`journalctl --vacuum-size`、清空应用自己的 log 目录、轮转策略）不属于清理动作：
   本程序既不执行，也不为它记账。日志目录要清理，只能作为普通 scope 的候选，走同一套规则与审核。
3. **应用服务变更**（`systemctl stop/disable/mask`、改配置、重启、退役服务）从不属于本程序的动作集合。
   它没有权限模型去表达"停一个服务"，隔离与清空只在自己 scope 内做 rename。服务停不停由变更流程决定，
   和磁盘清理分开授权、分开记录；一次清理没有停服务，不能写成"顺手把服务停了"。

一句话：**能自动做的只有 scope 内的移动与删除**；诊断是读，日志维护和 service 操作是另外两件事。

### 8.6 运行数据与采集：一个事实来源，两类视图，ZIP 不是备份

- **事实来源是结果页** `<state>/reports/latest.md` / `latest.json`（`cleanup setup --result-dir` 可改）。
  它写"已删除 / 待删除 / 保留"三区，另有「本轮审核」计数与每条候选的请求事实；数字只有这一套，
  `run` 的摘要和可读日志都从这里取，不各算一遍。成功与失败的审核都进汇总，按 `review_id` 去重；
  请求事实没记录下来的审核单独计 `review_facts_unknown`，**不按 0 补进 HTTP 尝试数**。
- **运行日志** `<state>/logs/dustkeeper.jsonl`（含 `.1`～`.3` 轮转）每次审核写一条无载荷记录：
  版本、commit、schema、`run_id` / `item_id` / `review_id`、`state`（pending / settled / refused /
  not_started / awaiting_request_check）、预算摘要、耗时、HTTP 状态、返回 token 用量与失败类别。
  不写 prompt、响应体、凭据或文件路径；`cmd/dustkeeper/review_diag.go` 是这份字段的唯一入口。
- **可读日志**是 `scripts/run-readable-log.py` 生成的一份人读文件：`latest-readable.md` 每次原子更新，
  按 `run_id` 留最近 10 轮快照，目录上限 20 MiB、每轮最多展示 200 条并写明总数与省略数，
  `schema_version=2`。旧页面缺字段时写"旧制品未记录"，不补 0；现在也会逐条写出关联 `review_id` 与
  "请求事实未记录"的审核数。用 `--run-json` 只格式化某一轮时会强制核对 `run_id`，不会拿上一轮冒充本轮。
- **诊断 ZIP** 用 `scripts/collect-run-data.py`（与二进制同一提交）：

  ```sh
  python3 /absolute/scripts/collect-run-data.py \
    --state-dir /absolute/confirmed/state \
    --output /absolute/private/dustkeeper-run-YYYYMMDD-HHMMSS.zip
  ```

  白名单只有标准位置的四份 JSONL 日志（当前 + `.1`～`.3`，每份最多 2 MiB + 64 KiB）与
  `reports/latest.json`（最多 4 MiB），源文件总上限约 12.25 MiB。输出是**新建的 0600 ZIP，绝不覆盖已有文件**；
  清单记录采集时间、每个文件的状态、大小、SHA-256 与"读取期间是否变化"。缺失、拒绝（symlink / 非常规文件）、
  不可读、超限都明确标成缺项，不冒充完整。脚本不运行 Dustkeeper、不调用模型、不上传、不改源文件。
  记住三件事：ZIP 只用于分析，**不是一致性恢复备份**（备份见 §11 的四件套），也不是供应商账本；自定义
  `result-dir` 不在白名单里，要另外保存实际结果页并标注来源与 `run_id`；采集期间程序若仍在运行，清单只能
  说明"观察到过变化"，不能保证整个包属于同一时刻。
- 结果页含业务文件名与路径，按实际分享对象处理。HTTP 尝试次数不证明供应商收到或计费；缺账单就写
  "费用未知"，不要填 0。

### 8.7 正式运行的分段耗时：`request_timings`

时序采集与调试用的 `ReviewBoth` 已经解耦：**正式运行也记录实际发出的请求**，
不需要、也绝不能把正式模式设成 `ReviewBoth`（那会在 JEV 保留之后仍调用 MiniMax）。
JEV → MiniMax 的短路、门槛、超时、预算以及模型调用次数、顺序、判断与额度都没有变化。

每条记录仍只有受限字段：`provider` / `outcome` / `phase` / `elapsed_ms` / `stages_ms`。
只为**确实发生过**的请求写记录；预算拒绝或本地拒绝没有请求时序，也不会生成"0 ms 请求"。
`stages_ms` 是相对请求开始的**事件时间点**，不是每阶段独立耗时：可读报告只在两个事件点都存在且可比较时
相减展示 DNS / 连接 / TLS / 首字节，连接复用、DNS 未触发等缺失事件写"未采集/不适用"或"阶段耗时未记录"，
**不补零**。旧数据缺字段写"未记录"，不推断成零耗时。

时序沿「审核结算事件 → 结果 JSON（`latest.json`）→ 运行日志 `dustkeeper.jsonl` → 可读报告」传递，
每条通过**实际 review ID** 关联；成功、超时与失败都有相应记录。结果页的人读说明哪个模型耗时、
最后观测的阶段、MiniMax 是否未调用。全程不记录 URL、代理地址、HTTP 头、正文、文件内容或凭据，
仍旧遵守日志轮转（2 MiB）与单字段 4 KiB 上限。`scripts/collect-run-data.py` 的白名单与总大小上限
保持不变，只是采集到的现有日志与结果页现在包含这些字段。

---

### 8.8 保留原因明细：`review_explanation`

此功能随 2026-11-10 的原因明细修复提供；本机是否已安装以 `docs/tasks/acceptance.md` 为准。
原 `reason` / `reason_text` 保持兼容，结果 JSON 新增可选 `review_explanation`，Markdown 的保留行显示其中文摘要：

- `local_facts_missing`：必要本地检查未完成；`local_move_safety_unknown`：本地检查尚不能确认移动安全；`model_insufficient_for_isolation`：本地可移动，但模型认为隔离依据不足。
- 本地摘要保留实际测得的检查布尔值、`facts_complete` 和 `movable`。未知值仍为 `null`，不按“没发现占用”处理。
- JEV 摘要记录选择、门禁是否通过及阻断码；MiniMax 摘要区分未调用、请求未取得判断、同意、保留和依据不足。
- MiniMax 的缺失依据仅允许用途或处置依据、引用关系、当前使用情况、可恢复性、敏感内容情况、来源、操作约束这 7 类。正式响应缺少该可选字段时仍兼容，记录为 `null`；显式空列表保留为 `[]`。模型没有列明具体缺项时，页面直接说明未列明，不替它推测。

示例：`模型认为隔离依据不足；本地必要检查通过；JEV：允许复核；MiniMax：依据不足；缺少依据：当前使用情况、可恢复性`。
这只是格式示例，不是对历史候选的补判。摘要由审核结算事件按实际 `review_id` 关联到结果页；keep 自身也记录相同摘要。
旧记录缺少这些字段时保留原样。摘要只保存已测布尔值和受限分类码，不保存模型自由文本解释、提示词、原始响应、凭据或文件正文。
这些字段不参与授权和处置计算；隔离/删除条件、原审核顺序及阈值保持不变。

同一受限摘要也写入 `dustkeeper.jsonl` 的 settled 审核日志（`review_explanation` JSON 字符串），按 `run_id` / `review_id` 关联；未开始或待核查记录不会冒充已有判断。原字段 4 KiB 上限及 2 MiB 轮转规则继续生效。
`run-readable-log.py` 自动报告升级为 schema 3，在既有动作、空间、耗时之后显示本地检查、JEV 门禁和 MiniMax 缺项。优先读取当轮结果页，旧页按既有事件关联兼容；仅保留分类码和布尔值，模型自由文本及错误自由文本不进入可读报告。`latest-readable.md/json` 与最近 10 轮快照仍由脚本自动生成，目录 0700、文件 0600。

---

### 8.9 elidli 无人值守入口

服务器已安装 `dustkeeper-elidli.timer`，每天 19:30 触发 `dustkeeper-elidli.service`；持久定时器在错过日程后补一次，无失败重试循环。手动运行也使用 `dustkeeper-elidli-run`，不要绕过自动入口直接跑核心二进制，否则不会经过这份预算账本。

自动入口 `scripts/run-scheduled-log.py --instance-root /var/lib/dustkeeper-elidli` 读取安装时显式创建的 `automation-budget.json`，缺失或损坏时拒绝且不会重置。elidli / macOS 预算各 35 元；每次先保留 0.01 元工程预算，0 HTTP 的新回执才退回，有模型调用则保留上界。费用字段不是实际扣款。上轮未结算、模型异常、费用未知、坏回执及超预算都暂停，不用下一次日程自动重试不明请求。

正常回执必须有新的 run ID、可解析且属于本轮的起止时间，以及明确的请求/失败/费用计数；部分完成只接受有明确“审核预算耗尽”证据的情况，其他部分失败暂停。结果仍自动写入既有 `readable-reports` 与 `state/logs`，可用 `systemctl list-timers dustkeeper-elidli.timer` 查下次日程，`journalctl -u dustkeeper-elidli.service` 查服务出口。

用户已批准服务器到期自动清空，但首次直接删除仍禁用；至少 7 天是下限，旧对象遵守原保留期和隔离时绑定的授权。暂停后先查本轮日志、pending 和预算记录，核实原因再恢复；不要删除账本、清空 pending 或改成 0 费用来强行续跑。

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

### 9.1.1 恢复之后：这个 scope 的自动授权要重新确认

`quarantine restore` 在写入恢复意图的数据库事务里做两件事：给恢复的目标路径加一条保护（`source=restore`），
并把该 scope 的 `protection_revision` 加 1——不必等到恢复真正完成。（这是数据库事务，不是横跨文件系统的
事务；文件移动与记账的关系见 9.2。）自动授权（`rule enable` 写下的 grant）绑定的是当时那个版本，于是：

- 恢复会让该 scope 绑定旧保护版本的自动授权失配。**检查到失配时，对应的自动动作被拒绝，并报告
  `GRANT_REQUIRED`（exit 4）。** 这是"授权不再匹配"，不是"规则错了"，也不等于把 scope 写成了 paused。
- **其它 scope 仍按各自授权执行**：它们的版本没有变。
- **本轮实际移动、删除了什么，以结果页为准，不能仅凭退出码认定整轮零动作。** `run` 是按 scope、按规则
  推进的：对账等步骤在此之前就已经发生；同一 scope 里若只重新确认了部分规则，仍有效的规则可能先执行，
  另一条规则随后才报旧授权失配。验收时见过"整轮 0 隔离 0 清空"的那一次，是那一次的结果，不是保证。
- 同一 scope 有多条规则时，受影响的是这个版本下的所有规则，需要逐条确认；只重新启用其中一条，
  不能宣称整个 scope 已恢复。

恢复之后的完整流程：

1. `quarantine restore --object <item_id>` 把需要的文件拿回来。程序新增的路径保护保留着
   （刚救回来的东西不该当天再被删）。
2. `rule list` / `rule show` 看清这个 scope 有哪些规则、各自的能力、模式与额度；
   `quarantine list --scope <id>` 看清观察区里还剩哪些对象。
3. 对每条受影响规则**按原来的能力与原额度**重新走一次 `rule enable` → 原样传回 digest：
   - 用原来的 rule ID 与 revision；
   - 原来启用的是 `cleanup`（到期清空），就显式写 `--mode cleanup`；省略默认是 `quarantine`，
     等于把它降成"只隔离"；
   - 保留原来的 `--max-files` / `--max-bytes` / `--purge-max-files` / `--purge-max-bytes`；
     省略会退回配置默认值，那不是原额度；
   - 先看不带 `--digest` 的提案，再用它输出的完整确认命令提交。
4. 新候选恢复自动清理。**这一步只覆盖新候选。**
5. 观察区里的旧对象**不继承新授权**：它们身上记的还是被撤销的那份旧授权。分别选择——
   - 继续保留（默认，什么都不做）；
   - `quarantine restore` 拿回来；
   - 到期后用既有的手动清空计划处理（`purge plan` / `purge apply`）；它要求当次明确计划，
     仍然检查保留期、时钟、保护、对象事实与双模型，不依赖旧自动授权是否还 active。

三条不要做：

- 不要用"自动重新启用所有规则"的脚本代替逐条决定——那等于一次性把所有规则的能力和额度重置成默认值。
- 不要为了"队列归零"新增批量无条件删除。没做完决定的对象继续留着是对的。
- 不要因为一次恢复就扩大目录、动作能力或额度。

这套机制是**有意保留**的设计（每个新保护都要求重新确认一次权限）。它不便宜，但把它改成"只保护被恢复
的那个对象"要连带处理旧计划、旧隔离对象绑定的授权与授权历史，属于换一套授权体系，不是改一个字段。

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
- 被 `held` 的 scope 有两条出路，取决于它为什么被 hold：
  - **还有未解决对象**（held / parked / 某个 intent / pending）：先处理那个对象（`quarantine list` 能看到它，
    `quarantine restore` 能把它拿回来）。这时 `scope resume` 一律拒绝，加旗标也一样。
  - **一个都没有**：说明 hold 已经不描述任何东西。这是旧版本留下的状态——补偿把对象放回去了，
    但 scope 没跟着放行，也没有 pending 留给对账。此时 `scope resume` 会说明这一点，加上
    `--accept-stale-hold` 才会执行。它只写 scope 那一行，树和对象都不动；下一次打开这个 scope
    仍会重新校验根身份，所以它不能用来绕过"根身份变了"的 hold——那种情况仍然要重新注册。

---

## 11. 备份

四样东西必须**一起**备份，缺一样就会在恢复时解释不通：

| 要备份的 | 为什么 |
|---|---|
| 配置目录（`<config>/config.json` 等） | 规则额度、时钟设置、保留期默认值在这里 |
| `<state>/state.db`，连同 `state.db-journal`（存在时） | 库用的是 `journal_mode=DELETE`，没有 `-wal`/`-shm`；漏掉日志文件会得到一个"差最后一步"的库 |
| 每个 scope 下的 `.dustkeeper/`（`owner.json`、`objects/`、`manifests/`、`durability.anchor`） | 隔离区里是原文件本身；清单记录移动前的身份 |
| 当前运行的二进制，以及它的 `version --json` 输出与 SHA-256 摘要 | 旧库只有对应版本的二进制解释得动；升级后回退要能核对拿回来的确实是同一份 |

只备份数据库会得到一堆指向不存在对象的记录；只备份隔离区则失去身份与保留期信息。**诊断 ZIP
（`scripts/collect-run-data.py` 的输出）不属于这四件套**：它只有日志与结果页，用于分析，不能用来恢复一致性状态。

```sh
# 1) 先停调度，再等进程走完。pause 只阻止新的 intent，不阻止定时任务再拉一次 run。
systemctl --user disable --now dustkeeper.timer      # macOS: launchctl bootout gui/"$(id -u)" ...
dustkeeper pause
dustkeeper status                                    # pending_operations 必须为 0，且没有 run 在跑
lsof "$STATE/instance.lock"                          # 应当没有输出：有输出说明还有进程持有这把锁

# 2) 库静止后按原权限复制数据库与现存 sidecar（不要 cp 一个正在写的文件）
#    本项目用 journal_mode=DELETE；若出现非空 -journal/-wal/-shm，先保留现场，
#    用旧二进制正常打开关闭，再按已有恢复路径处理，绝不删 sidecar。
cp -p "$STATE/state.db" /backup/state.db
for s in -journal -wal -shm; do
  [ -e "$STATE/state.db$s" ] && cp -p "$STATE/state.db$s" /backup/state.db$s
done
shasum -a 256 /backup/state.db > /backup/state.db.sha256

# 3) 配置、隔离区、二进制与部署参数
tar czf /backup/dustkeeper-config.tgz -C "$CONFIG" .
tar czf /backup/vault-exports.tgz -C /srv/app/disposable-exports .dustkeeper
cp -p /usr/local/bin/dustkeeper /backup/dustkeeper-1.0.0-phase1-linux-amd64
dustkeeper version --json > /backup/version.json
shasum -a 256 /usr/local/bin/dustkeeper > /backup/dustkeeper.sha256
```

> 上面的 `systemctl` / `/usr/local/bin` / `/srv/app/...` 是**跨平台模板**，不是本机命令。本机托管实例
> 的调度单元是 macOS launchd 的 `com.dustkeeper.managed`，配置文件是
> `~/Library/LaunchAgents/com.dustkeeper.managed.plist`，二进制在 `~/.local/bin/dustkeeper`，实例目录在
> `~/Library/Application Support/Dustkeeper-Managed/`。实际执行时**只停这一个已确认的 label**，不停止其他
> launchd 或用户服务；先用 `launchctl print "gui/$(id -u)/com.dustkeeper.managed"` 确认状态，不强杀进程。

**关于系统 `sqlite3 .backup`**：旧模板写过 `sqlite3 "$STATE/state.db" ".backup ..."`，但本机系统自带的
`sqlite3` 版本能否解释本项目的 STRICT 表、`PRAGMA` 与当前 schema **没有在所有本机环境上验证过**，
因此它不能当作通用备份命令写进步骤。上面的口径与 §3.1 的显式升级说明一致：先停调度、等进程退出、
确认 `pending=0` 且无进程持有实例锁，再复制**同一时点**的库、sidecar、完整 config、每个 scope 的
`.dustkeeper/`、旧二进制、`version --json` / SHA-256 与实际 plist。配置含凭据，只在本机按原权限或更严格
权限保存，不贴进报告、不上传。诊断 ZIP 不属于一致性恢复备份。

`status` 里只要还有 pending 操作，就先跑 `recover reconcile` 再备份。备份一个"半途"状态，恢复时会更难解释。
数据库升级后不能只换回旧二进制（旧程序可能不认识新 schema）；一旦有新隔离、恢复或永久删除，
禁止盲目覆盖旧数据库或旧 vault，先核对真实文件与事件再制定恢复步骤。

**副本不是沙箱。** 把 `DUSTKEEPER_STATE_DIR` 指向备份、然后对着它跑 `run` 或 `quarantine restore`，是很危险的
动作：库里的 scope 根路径仍然是原目录的绝对路径，副本会照着这些路径去动真实文件。恢复演练要么在一个真正
隔离的测试目录里做（重新 `scope add` 那个测试目录），要么只做只读命令（`status`、`events`、`recover inspect`）。

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
| `quarantine restore` 之后 `run` 报 `GRANT_REQUIRED` | 预期行为：恢复让该 scope 的自动授权版本失效。按 9.1.1 逐条重新确认；旧隔离对象不继承新授权 |
| 系统自带 `sqlite3` 打不开 `state.db`（`near "STRICT": syntax error`） | 系统客户端太旧（STRICT 表要 3.37+）。程序内嵌 SQLite 3.53，用 `status` / `events` / `cleanup report` 查看，不要用旧客户端改库 |
| 服务器上整轮 `MODEL_UNAVAILABLE` | 先看 JEV 出口（直连返回 451 说明被地域策略挡住，需要 `jev_proxy_url`），再确认 `lsof` 已安装、`trust_ca_file` 路径无符号链接 |
| 计划刚生成就 `PLAN_STALE` | 有进程在写该目录（父目录身份或 mtime 变了）；`--writers-stopped` 之后重建计划 |
| `plan apply` 报 `INVALID_ARGUMENT` | `assisted` scope 的手工计划必须显式 `--writers-stopped`：先确认没有进程在写这棵目录 |
| `purge apply` 报 `DIGEST_MISMATCH` | 计划内容在生成后变了；`purge show` 会说明重算摘要是否与存档一致 |
| 隔离区一直增长 | `auto_purge` 没开，或保留期还没到；`purge plan` 会列出到期对象 |
| `INSTANCE_BUSY` 反复出现 | 有残留实例或另一个定时任务；`ps` 查一下，锁文件在 `<state>/instance.lock` |
| `run` 每个对象都是"left alone: protected" | 该路径被 `protect add` 过或刚 restore 过；这是有意的，不会自动过期 |
| `run` 报 `notify_failed` 且对象没被清空 | 设计如此：提醒没送达就不清空。看 `events` 里 `action=notify` 的 detail，修好通知出口后下一轮会自动重试 |
| `cleanup report` 说没有结果页 | 还没有跑过 `run`；页面由每轮 `run` 写在 `<state>/reports/latest.md`（或确认策略时的 `--result-dir`） |
| `cleanup setup --digest` 报 `DIGEST_MISMATCH` | 两次调用之间参数改过；重新预览一次，把新的 digest 原样传回去 |
| macOS 上 `EXC_BAD_ACCESS` / `EBADF` | 权限授予对象错了（授予终端而非二进制）；或磁盘异常 |
| 时间相关测试失败 | 检查时区与 `TZ`；程序内部全部用 UTC 纳秒 |
| `ACCESS_DENIED` 但 `ls` 正常 | 程序用 fd 相对路径 + `O_NOFOLLOW`，不跟符号链接；检查路径中是否有链接段 |
