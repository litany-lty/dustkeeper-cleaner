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

调低多少是风险决定，不是配置问题：同一组样本在 70/80 下四次里只有一次两方都同意。`timeout_seconds`
（5..120，默认 30）控制单次模型请求的等待；实测 20s 会让 MiniMax 偶尔超时，超时会被记成"模型不可用"
并跳过该文件，而不是等它。

**自签或内网端点：`trust_ca_file` 必须是一条全程没有符号链接的真实路径。** 凭据与 CA 都是逐段
`O_NOFOLLOW` 打开的，路径里只要有一段是链接就直接 `MODEL_UNAVAILABLE`，而那个错误信息不会指向路径。
macOS 上 `/tmp`、`/var` 本身就是链接，Linux 上不少发行版的 `/home` 也是；先 `realpath` 再用解析后的路径
写进配置。

**外网 JEV 需要目标机自己有一条可用的出口。** 直连被地域策略挡住时（HTTP 451），JEV 那一路先失败，
整轮退化成"模型不可用"、一个文件都不动。出口填 `review.jev_proxy_url`（只接受 `http://host:port`，
不带用户名密码、query 或路径，程序不做直连回退）；MiniMax 不走这条代理。程序不会凭空提供代理出口，
服务器上没有可用的 HTTP CONNECT 服务就得先装一个。

页面里的"已删除"只认**已完成并确认**的动作：候选、模型意见、intent、debug 模拟都不计入。超过 200 条时
显示总量与"还有 N 条未显示"。`cleanup report` 是只读命令：它读文件，不移动、不删除、也不写审计。

仓库里另有一个独立工具 `cmd/cleanup-report`，那是**只读目录盘点**（看使用痕迹、已知引用、空间上限），
不读状态库、不生成许可，和这里的 `cleanup report` 不是一回事。

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

`quarantine restore` 在写入移动的同一个事务里做两件事：给恢复的目标路径加一条保护（`source=restore`），
并把该 scope 的 `protection_revision` 加 1——不必等到恢复真正完成。自动授权（`rule enable` 写下的 grant）
绑定的是当时那个版本，于是：

- **这个 scope 的下一次 `run` 会在动作前比对失败，整轮以 `GRANT_REQUIRED`（exit 4）停下**，一个文件都不动。
  这是"授权不再匹配"，不是"规则错了"，也不等于把 scope 写成了 paused。
- **其它 scope 不受影响**：它们的版本没有变。
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
| 当前运行的二进制 | 旧库只有对应版本的二进制解释得动；升级后回退需要它 |

只备份数据库会得到一堆指向不存在对象的记录；只备份隔离区则失去身份与保留期信息。

```sh
# 1) 先停调度，再等进程走完。pause 只阻止新的 intent，不阻止定时任务再拉一次 run。
systemctl --user disable --now dustkeeper.timer      # macOS: launchctl bootout gui/"$(id -u)" ...
dustkeeper pause
dustkeeper status                                    # pending_operations 必须为 0，且没有 run 在跑
lsof "$STATE/instance.lock"                          # 应当没有输出：有输出说明还有进程持有这把锁

# 2) 数据库用 SQLite 自己的备份，不要 cp 一个正在写的文件
sqlite3 "$STATE/state.db" ".backup '/backup/state.db'"

# 3) 配置、隔离区、二进制
tar czf /backup/dustkeeper-config.tgz -C "$CONFIG" .
tar czf /backup/vault-exports.tgz -C /srv/app/disposable-exports .dustkeeper
cp /usr/local/bin/dustkeeper /backup/dustkeeper-1.0.0-phase1-linux-amd64
```

`status` 里只要还有 pending 操作，就先跑 `recover reconcile` 再备份。备份一个"半途"状态，恢复时会更难解释。

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
