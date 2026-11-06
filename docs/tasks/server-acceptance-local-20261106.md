# 目标机验收（本机代跑）：e78057e 的安装、独立目录混合批次与调度

日期：2026-11-06（Asia/Shanghai）
制品：`dustkeeper-1.0.0-phase1-darwin-arm64`，CI run `42057572813`，commit `e78057e`
对应复核文档：《Linux 正式服务器上线复核与最后修补清单》（`dustkeeper-server-readiness-25dcee2-20261105`）第 8 节第二步、第三步

> 没有可用的 Linux 目标机，本轮按「目标机器先以本地机器代替」执行：安装、独立状态与 scope、
> 只读检查、混合批次、调度上下文执行都在本机 macOS arm64 上完成。
> **因此本文证明的是「这份制品在这台机器上按文档步骤走通」，不证明 linux-amd64 目标机、
> ext4/XFS、systemd、目标账号下的模型网络。** 那几项仍按第 7 节在真实服务器上核对。
>
> 后续（2026-11-06）：后两项已在真实 Linux 服务器上补做，见 [server-acceptance-linux-20261106](server-acceptance-linux-20261106.md)。

## 1. 安装与制品核对

安装位置：`/Users/litany/dustkeeper-acceptance-20261106/bin/dustkeeper`（目录 0700，文件 0700，
独立于 `~/.local/bin` 的已安装实例，未改动后者）。

| 核对项 | 结果 |
|---|---|
| `shasum -a 256` 二进制 | `e5c7b0c8536397027f74b3385bf9ff15408d4ffa17ffed41845875e0d6a0b0bb` |
| manifest 中的 `sha256` | 同上，逐字符一致 |
| manifest 中的 `commit` | `e78057e` |
| `dustkeeper version` | `dustkeeper 1.0.0-phase1 (darwin/arm64, SQLite 3.53.4, commit e78057e)` |
| `file` | `Mach-O 64-bit executable arm64` |

`1.0.0-phase1` 是版本字符串，识别安装应以 commit + 制品摘要为准，与复核文档 2.1 一致。

两个独立实例，互不共享状态，均不读取任何生产库：

```text
A-failclosed/   state/ config/ tree/ out/    未配置审核，用于验证「没有模型就不动文件」
B-mixed/        state/ config/ tree/ out/    配置审核（本地假端点），用于混合批次与调度
pki/            ca.pem info.json             假端点自签 CA
schedule/       com.dustkeeper.acceptance.plist  README.txt
```

## 2. 只读检查

`doctor`（实例 A，`--scope accept`）：

```text
ok   state_dir                私有状态目录存在
ok   database                 1 scopes, 0 rules, 0 items
ok   capabilities_state_dir   独占重命名、目录同步、不跟随符号链接、完整设备屏障均验证
ok   pause_marker             running
ok   pending_operations       0 operation(s) need reconciliation
ok   clock                    no previous run recorded
ok   scope accept             active
```

scope 根上的能力探测（APFS，本机）：

```json
{"platform":"darwin","no_replace_rename":true,"dir_sync":true,"full_barrier":true,
 "xattr_read":true,"file_flags":true,"acl_query":true,"no_follow":true}
```

`scan --scope accept`：`dirs=1 files=4 bytes=66 incomplete=false`，四个测试文件全部读入。

两点应当记下来，属于第 7 节条件的现状而不是本轮缺陷：

1. **`doctor` 没有模型检查项。** 它检查目录、数据库、文件系统能力、暂停标记、待对账、时钟，
   不检查 `review` 配置、凭据文件、CA 或端点连通性。目标机上「先跑只读的环境与模型检查」的模型
   那一半，只能用 `debug --root <dir> --execute`（见 5.3）或一次真实 `run` 来证明。
2. **`trust_ca_file` 不能经过符号链接。** 凭据与 CA 都按「逐段不跟随符号链接」的方式打开，
   第一次把 CA 放在 `/tmp/...` 直接得到 `MODEL_UNAVAILABLE`（`/tmp` 在本机是指向
   `private/tmp` 的符号链接）。把 CA 移到 `/Users/litany/dustkeeper-acceptance-20261106/pki/ca.pem`
   后立即正常。服务器上同样要选一条全程无符号链接的真实路径。

## 3. 混合批次（实例 B）

`tree/` 里四个合成文件（内容为固定字符串，其中三个 mtime 为 2020-01-01）：

| 文件 | 类别 | 期望 |
|---|---|---|
| `removable.tmp` | `temporary_name_only`，在策略的直接删除类别里 | 实际删除 |
| `observable.zip` | `archive_unknown_contents` | 进入观察区，可列出、可恢复 |
| `declined.zip` | 同上，模型拒绝 | 原地不动、字节不变 |
| `recent.tmp` | 未满 24 小时门槛 | 根本不进计划 |

确认策略（`cleanup setup` 两段，摘要确认）：

```text
--scopes mixed --categories temporary_name_only --observation-days 7
--max-files 5 --max-bytes 1073741824 --notify none --keep-retry-days 0
--result-dir .../B-mixed/out
```

规则：`*.tmp`、`*.zip`，`min_mtime_age_seconds=86400`，`retention_seconds=604800`，
`rule enable --mode cleanup` 生成 Q/P 两条授权（各 100 文件 / 1 GiB，取自配置默认）。

一次 `run` 的结果（exit 0）：

```text
已删除（1）removable.tmp   18 B   双模型同意永久删除
待删除（1）observable.zip  16 B   最早清空 2026-11-13 00:13
保留（1）  declined.zip    17 B   模型明确要求保留
```

结果页给出的恢复命令被原样执行（只用页面上的那一行，不手写对象 ID）：

```text
dustkeeper quarantine restore --object obj_3ff117464fd6f7f90b9a75a3bdb875b1
→ {"restored":"observable.zip","protected":true}
```

恢复后：目录内三个文件都在，字节与投放时一致（`archive payload\n` / `declined payload\n` /
`recent payload\n`），`removable.tmp` 确认不存在，`quarantine list` 计数回到 0。

假端点计数 `{"jev":3,"minimax":2}`：JEV 问了三个候选，MiniMax 只在前两个放行后各问一次——
被拒绝的那一个没有进入第二层，这是双模型门禁的顺序，不是漏问。

## 4. 没有模型时不动文件（实例 A）

同一套策略与规则，但 `config.json` 没有 `review` 段：

```text
run exit=6  code=REVIEW_REQUIRED
accept: REVIEW_REQUIRED: retained: configure dual review before any quarantine or deletion
quarantined_total=0 purged_total=0
```

目录内四个文件字节不变，`cleanup report` 三段计数均为 0。也就是说「缺审核」是一轮拒绝，
不是一个默认放行。

## 5. 调度

### 5.1 模板

`schedule render --platform darwin --label com.dustkeeper.acceptance` 生成了 plist，
`plutil -lint` 通过，内容是文档描述的形状：`ProgramArguments` 为数组（可执行文件 + `run`）、
`EnvironmentVariables` 带 `DUSTKEEPER_STATE_DIR`/`DUSTKEEPER_CONFIG_DIR`、
`StartCalendarInterval` 定时、`StandardOutPath`/`StandardErrorPath` 指向 `<state>/run.log`、
没有 `KeepAlive`。

### 5.2 本会话无法注册 launchd 作业

`launchctl bootstrap gui/501 <plist>` 与 `user/501` 都返回 `Bootstrap failed: 5: Input/output error`；
换成一个只有 `/bin/echo` 的最小 plist 同样失败，`launchctl submit` 返回 `Bad request`，
`crontab` 返回 `operation not permitted`。plist 本身 `plutil -lint` 为 OK、权限 644、owner 正确。
**结论：这个会话（托管/远程登录环境）不允许注册调度作业**，不是模板或制品的问题。
因此「launchd 真的把我们叫起来」这一环没有在本机得到证据，我也没有把它写成已通过。

### 5.3 用调度器的运行环境执行同一条命令

可验证的部分按 plist 的 `ProgramArguments` 与 `EnvironmentVariables` 逐字复现，并把环境清空到
调度器给的样子（只有 `HOME` 与 `/usr/bin:/bin:/usr/sbin:/sbin`，没有交互式 shell 的变量、
没有代理变量），输出重定向到 plist 指定的 `run.log`：

```text
env -i HOME=$HOME PATH=/usr/bin:/bin:/usr/sbin:/sbin \
    DUSTKEEPER_STATE_DIR=.../B-mixed/state DUSTKEEPER_CONFIG_DIR=.../B-mixed/config \
    .../bin/dustkeeper run > .../B-mixed/state/run.log 2>&1
```

第一次执行返回 **exit 4 / GRANT_REQUIRED**，原因见第 6 节 F1。
按报错自己的提示重新 `rule enable`（两段摘要）后，同一命令：

```text
run run_7bc4558b6f68c8287b2ddc3c7ced1376
  reconciled 0 operation(s)
  quarantined 1 object(s), purged 0 object(s), read 36 bytes
  this run: 0 deleted, 1 moved to the observation area, 1 left alone
  result page: .../B-mixed/out/latest.md
  mixed   scanned=4 complete=true quarantined=1 purged=0
      notifications are off: notifications are turned off in the confirmed cleanup policy
exit 0
```

结果页被这次运行重写（`生成时间 00:13:19`，`待删除 scheduled.zip`，恢复命令带新对象 ID），
`quarantine list` 里就是 `scheduled.zip` 一个对象、`restorable=true`。
通知按确认策略是 `none`：运行摘要写明「notifications are off」，没有把用户的选择记成故障，
也没有调用系统通知。

## 6. 本轮新发现

**F1（运维语义，未改代码）一次恢复会让该 scope 的自动授权失效。**
`quarantine restore` 会新增一条保护记录并让 `scopes.protection_revision` 加 1；
授权绑定里带着 `protection_revision`，于是下一轮 `run` 在动作前比对失败，
整轮停止并返回 `GRANT_REQUIRED`（`internal/plan/grant_check.go:12`），
必须重新 `rule enable` 才能继续。本机实测：restore 前 revision=1，restore 后=2，
调度上下文第一次 `run` 因此 exit 4。

这个行为与「新增保护要重新审一次权限」的设计一致，我不认为应当在不讨论的情况下改掉；
但它对运营的影响是具体的：**只要有人在结果页上恢复一次文件，自动清理就会停到下一次
人工重新启用为止**。第 4 步进入受控正式使用之前需要明确：这是期望的（每个恢复都要求重新确认
权限），还是希望只让「被恢复的那个对象」进入保护而不动整份授权。本轮没有改。

**后续决定（2026-11-06）：保留这个行为**，不修改保护版本、授权校验或历史对象绑定。完整后果与恢复后的
操作流程写进 [运维手册 9.1.1](../operations.md)：按原能力、原额度逐条重新确认受影响的规则；
重新确认只覆盖新候选，观察区里的旧对象不继承新授权，可以继续保留、恢复，或到期后用既有的手动清空计划处理。

**F2（配置路径）`trust_ca_file` 必须是全程无符号链接的真实路径。**
见 2.2。写进部署清单可以省掉一次 `MODEL_UNAVAILABLE`。（已作为部署注意写进[运维手册第 8 节](../operations.md）。）

**F3（诊断能力）只读模型检查不是 `doctor`，而是 `debug --root <dir> --execute`。**
本轮用假端点跑 `debug --execute --advisory` 得到 `MODEL_UNAVAILABLE`：advisory 走的是
`Judge.Review` 的另一套问答，假端点只实现 triage 的固定答复，应答形状对不上。
这不是产品缺陷，但它意味着**「目标机的模型链路」这一项本轮只能算未验证**——
真实双模型端到端仍要一次真实端点调用。（`doctor` 不含模型检查这一条已写进[运维手册第 5 节](../operations.md)；
真实双模型链路已在 [Linux 目标机验收](server-acceptance-linux-20261106.md)里打通。）

## 7. 与停止条件的对照

| 停止条件 | 本机代跑后的状态 |
|---|---|
| 三个已复现缺陷关闭、目录同步与 none 接线补齐 | 已完成（`e78057e`，回归先在未修复代码上失败过） |
| 同一最终提交的 CI/制品通过 | 已完成（run `42057572813` 两平台绿，两侧 manifest 均为 `e78057e`） |
| 目标机小目录实际模型闭环 | **部分**：安装、独立目录、只读检查、混合批次、恢复、结果页都在本机走通；模型是本地假端点，真实 JEV+MiniMax 未验证（本机无 MiniMax 凭据，历史记录该网关曾返回 401） |
| 调度通过 | **部分**：模板正确，调度上下文里的命令 exit 0 且写出结果页；launchd 作业注册被本会话环境拒绝，systemd 未在 Linux 上跑 |
| 结果可查、主动通知按选定方式满足 | 结果页可查（`cleanup report` 在清空环境里也能读）；选定 `none`，不承诺主动送达 |

## 8. 结论

在本机能证明的部分，`e78057e` 的行为与文档要求一致：缺审核整轮拒绝、混合批次按四个类别分别
收敛、恢复命令可用、结果页与观察区清单互相吻合、策略选定 `none` 时运行摘要如实说明。
本机不能证明的部分（linux-amd64 + ext4/XFS + 目标账号 + systemd + 真实双模型）仍然要在服务器上
按第 7、8 节走一遍；在那之前，「已部署/已验收」这个说法依然不成立，正式实例继续保持暂停。

## 9. 复现步骤（本机）

```sh
ROOT=/Users/litany/dustkeeper-acceptance-20261106
B=$ROOT/B-mixed
DK=$ROOT/bin/dustkeeper
export DUSTKEEPER_STATE_DIR=$B/state DUSTKEEPER_CONFIG_DIR=$B/config
$DK doctor --scope mixed
$DK scan --scope mixed
$DK cleanup setup --scopes mixed --categories temporary_name_only --observation-days 7 \
   --max-files 5 --max-bytes 1073741824 --notify none --keep-retry-days 0 \
   --result-dir "$B/out"            # 先看摘要，再加 --digest 确认
$DK rule import --file "$B/rule.json" --id mixed-rule
$DK rule enable --id mixed-rule --revision 1 --mode cleanup   # 再带 --digest 提交
$DK run
$DK cleanup report                # 页面上的 restore 命令可直接执行
$DK quarantine list
```

假端点是一次性的（`/tmp/dk-fake-review`，未进仓库）：自签 CA、TLS 1.2 以上、
按请求里的 `relative_path` 分文件作答、对 `declined.zip` 只给 `keep`。
它只用于让清理链路走完，不代表模型判断质量。
