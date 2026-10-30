# 目标机器上的有限部署验收（2026-10-30，macOS 26.5.2 / arm64）

一次真实闭环记录，不是测试报告。范围固定为：**一个专门用来丢东西的小目录**，用最终制品跑完
扫描 → 批准 → 隔离 → 查看 → 恢复 → 再跑两轮。长保留期之后的真实永久删除**没有**在这次里发生，
也不声称发生过。当天晚间补齐同一固定制品在独立 scope 下的三轮 launchd 日历触发验收，见 §5。

## 1. 用的东西

| 项目 | 值 |
|---|---|
| 主机 | macOS 26.5.2（build 25F84），arm64；APFS，`/tmp` 在数据卷上，可用 67 GiB |
| 制品 | `dustkeeper-1.0.0-phase1-darwin-arm64`，CI run `41022348788` 的 artifact `12962491110`（3 981 691 B），`commit=e54c9e8` |
| checksum | `sha256=da4ebe56d951c6c226d8c97a61901f02ec28eb4fc0a6a09540849e0dc9555c82`（`shasum -a 256 -c SHA256SUMS` 通过） |
| 构建方式 | CI `macos-latest`（arm64）的 `./build/build.sh --release`，即交付用的那份字节 |
| 状态/配置 | `/tmp/dk-accept-ci/{state,config}`，与真实安装完全分离 |
| 测试目录 | `/tmp/dk-accept-ci/tree`，5 个文件 / 2 个目录，共 44 KiB |

先用本机 `./build/build.sh --release` 的产物（`commit=fca60b8`，`sha256=303ad908…d143`）跑过同一串步骤，
结果与下表逐项相同；换成 CI 制品重跑一遍是为了让记录里引用的是**要交付的那份字节**，而不是我本机编的
一份（两者 checksum 不同只因为嵌了不同的 commit 与构建时间）。

目录内容（40 天前的 mtime 用 `touch -t` 设成 2026-09-20 09:00）：

| 文件 | 预期 | 结果 |
|---|---|---|
| `old-match.zip`（18 B，40 天前） | 命中规则，被隔离 | 隔离，恢复后逐字节相同 |
| `fresh.zip`（14 B，今天） | 不到 30 天，不动 | 未动 |
| `keep/keep.zip`（13 B，40 天前） | 命中 `except.relative_dir_prefixes` | 未动 |
| `发票-2024.zip`（16 B，40 天前） | 命中 `except.basename_patterns` | 未动 |
| `notes.txt`（10 B，40 天前） | 不是 `*.zip`，不动 | 未动 |

规则是手写离线 JSON：`match` 为 30 天未修改的 `*.zip`，`except` 为 `keep/` 与 `*发票*`，
`retention_seconds=2592000`，`--mode quarantine`（默认）。

## 2. 实际执行的命令与结果

```text
version                 1.0.0-phase1  commit=e54c9e8  schema=3
init                     code=OK
scope add                code=OK  scope_id=accept  kind=managed_disposable  vault=true
scan --scope accept      code=OK  dirs=2 files=5 incomplete=false
rule import              code=OK  digest=sha256:fb79e41c…
rule enable（提案）       code=OK  mode=quarantine  matched=1  enable_digest=sha256:d47074e1…
                                  候选只有 old-match.zip（truncated=false）
rule enable --digest     code=OK  grants=[auto_quarantine]  max_files=100  max_bytes=1 GiB
run                      code=OK  control=continue  scanned=5  quarantined=1  purged=0
status                   code=OK  observation_count=5  quarantined_count=1  due_for_purge_count=0
quarantine list          relative_path=old-match.zip  due=false  auto_purge_granted=false
quarantine restore       code=OK  restored=old-match.zip  protected=true
run（恢复后第 1 轮）      code=OK  scanned=5  quarantined=0  purged=0
run（恢复后第 2 轮）      code=OK  scanned=5  quarantined=0  purged=0
```

`doctor --scope accept` 在此之前跑过，六项检查（state_dir / database / capabilities_state_dir /
pause_marker / pending_operations / clock）全为 True——那是本机构建那次记录的；同一台机器、同一个
临时目录形状，CI 制品重跑时没有出现不同的检查结论。

本机构建那次的 `run` 还带 `quarantined_logical_bytes=18`、`read_bytes=36`、`clock_trusted=true`，
与上表逐项一致。

两次运行的 `instance_id` 与隔离对象 ID 都不同（`inst_69c0b721…` / 另一个；`obj_9ef42e…` / `obj_ac7852…`），
它们是每次初始化、每次隔离新生成的名字；其余输出一致。

隔离区 `objects/` 里只有那一个对象，恢复之后该目录只剩 `.dk-vault-nonce`。恢复时返回 `protected: true`，
随后两轮都是 `quarantined=0`：**刚恢复的文件没有被下一轮立刻再挑中**，这是既有行为，不是这次改出来的。

`cmp` 逐字节比对恢复后的 `old-match.zip` 与原内容：相同。

## 3. 资源与体积（同一台机器，单 scope，5 个文件）

| 指标 | 实测（CI 制品，稳定态一轮） |
|---|---|
| `run` 墙钟 | 0.15 s（`/usr/bin/time -l`，已隔离过的稳定态；首轮含隔离写入是 0.2 s 量级） |
| 峰值 RSS | 15 384 576 B ≈ 14.7 MiB（`maximum resident set size`） |
| `state.db` | 180 KiB（含扫描、规则、授权与事件） |
| `.dustkeeper/` | 24 KiB（`objects/` 恢复后为空） |
| 日志 | `state/logs/dustkeeper.jsonl` 8 KiB，轮转未触发 |

这几个数字只说明"这么小的目录跑得动、内存没有失控"，**不能**外推到几十万文件；10 万文件的历史测量在
`docs/evidence/performance-100k.md`，那份数据来自 `e14855c`，不是本次制品。

## 4. 永久清空这一次没有发生，也不假装发生

`quarantined_count=1` 的同时 `due_for_purge_count=0`：保留期 30 天还没到，所以
`run` 没有任何东西可清空（`purged=0`）。这正好是我们要的"未到期不删"，但反过来那一半
（到期 + 有独立授权 → 真的删掉）只由受控时钟的用例覆盖，本次没有靠改系统时间或改库里的
`purge_not_before_ns` 去凑一个 P：

- `test/integration`：`TestManualPurgeRemovesOnlyListedObjects`（到期前拒绝、到期后只删计划内的）、
  `TestAutomaticPurgeNeedsItsOwnGrant`（后来开的 auto-purge 不追溯覆盖旧对象）等，本轮全量通过。
- 三次短期运行也不可能"观察完"一个 30 天的保留期，记录里不这么写。

## 5. 调度：初次失败记录与后续三轮验收

**初次尝试（保留历史记录）：**

`schedule render --platform darwin --hour 3 --minute 30` 生成的 plist 内容与预期一致：`ProgramArguments`
是当前二进制的绝对路径，`EnvironmentVariables` 指向本次的 state/config，`StandardOutPath` /
`StandardErrorPath` 都是 `state/run.log`，没有 `KeepAlive`。

当时**没有做到"由实际 launchd 身份启动"**。按 `docs/operations.md` 的两条路都试过，都失败：

| 做法 | 结果 |
|---|---|
| `launchctl bootstrap gui/501 ~/Library/LaunchAgents/dustkeeper.plist` | `Bootstrap failed: 5: Input/output error` |
| `launchctl bootstrap user/501 …` | 同上 |
| `launchctl submit -l dk-accept -- …` | 无输出、`launchctl list` 里没有该作业 |

`launchctl managername` 报告 `Aqua`；当前记录能确认 bootstrap 返回错误 `5: Input/output error`，
但没有足以确认权限问题或其它根因的日志，**失败原因尚未定位**。装进去的 plist **已经删掉**，
`launchctl print gui/501/dustkeeper` 仍然是 `Could not find service`，主机上没有留下任何 dustkeeper 作业。

因此初次记录的结论是：**本机 launchd 调度已尝试，但验收未通过。** plist 的字段、路径与命令是核对过的，
模板生成的逻辑有 `internal/schedule` 的用例覆盖，但当时没有用它顶替真实调度。

**2026-10-30 晚间补验收：三轮通过。**

Mac 为 macOS 26.5.2 / arm64，运行身份为 uid 501、`gui/501`。重新从 CI run `41022348788` 的 artifact
`12962491110` 下载固定候选 `e54c9e8`，校验完整 SHA-256 与 §1 相同，未重新构建。复用
`/tmp/dk-accept-ci/{state,config,tree}`，只登记既有 `accept` scope 与五文件，`model.enabled=false`。

由现有 `schedule render` 生成独立 label `com.dustkeeper.acceptance.20261030.rrpuwfvl`，
`ProgramArguments` 仍为绝对制品路径加 `run`；仅在验收副本将 `StartCalendarInterval` 设为下面三个时间点，
其余键逐项与原模板相同。无 `RunAtLoad` / `KeepAlive`，没有用 `kickstart`、`submit` 或手工 `run` 代替触发。
`bootstrap` 退出 0；以下均为北京时间（UTC+8），result 时间来自实际完成记录：

| 日历计划时刻 | result 时间 | launchd runs / last exit code | 运行结果 |
|---|---|---|---|
| 19:22 | 19:22:06.148911 | 1 / 0 | `code=OK`，`exit=0`，`control=continue` |
| 19:23 | 19:23:06.708646 | 2 / 0 | `code=OK`，`exit=0`，`control=continue` |
| 19:24 | 19:24:09.514185 | 3 / 0 | `code=OK`，`exit=0`，`control=continue` |

三轮 runner 的 `run_id` 分别为 `run_fb752ede757caf41456bcc217ed8a103`、
`run_6c86720b499d96d0317cfbf89b5193ee`、`run_a3181539a9213510bf1c72a66ea75340`；这里记录的是 runner ID，
不是 CLI result 的 ID。每轮 scope 数为 1、`scopes_not_reached=0`、Q/P 均为 0、failures 为空。
五文件的 SHA-256、inode、mtime、size 在三轮前后逐项相同，配置 SHA-256 未变；随后 `status` 返回 OK：
observations=5、pending=0、quarantined=0、due=0、protections=1、scope=active。

验收完成后 `bootout` 退出 0，`launchctl print` 确认作业找不到，验收 plist 已移除。
**本次关闭的是独立测试 scope 的定时启动验收；未保留正式自动清理任务。**
错误 5 本次未复现，但不能反推出初次失败的根因；产品代码与系统隐私授权均未因本次验收而修改。

Mac 上的原始记录保留在 `/tmp/dk-launchd-20261030-rrpuwfvl/`：`summary.json`、`bootstrap.json`、
`job-loaded.json`、`job-after-1.json`、`job-after-2.json`、`job-after-3.json`、`job-final.json`、
`bootout.json`、`job-after-cleanup.json`、`scheduled-records.jsonl`、`files-before.json`、
`files-after.json`、`status-after.json`。这些是本机临时目录中的现场记录，仓库保留上述摘要与固定制品标识。

systemd 一侧仍未执行：本次没有 Linux 目标机。`linux-amd64` 的制品由 CI 的 `ubuntu-latest` 产出并冒烟，
容器里跑的是命令序列，不是 systemd timer。

## 6. 这次验收覆盖了什么、没覆盖什么

覆盖到：

- 最终制品在真实机器上完成 init → scope add → doctor → scan → rule import → enable 提案 → 确认 → run →
  quarantine list → restore；
- 只有匹配且未被排除的文件被移动；排除样本、保护样本与不相干文件原样不动；
- 恢复后字节一致、原路径不被覆盖，且自动加了路径保护，后续两轮没有再选中它；
- `status` 的三个计数与 `quarantine list` 的 `due/auto_purge_granted` 与事实一致；
- 同一固定制品在独立 scope 中由 `gui/501` 的日历自然触发三轮成功，五文件与配置前后不变；验收任务已卸载；
- 资源占用落在预期量级。

没覆盖到（如实记录，不用别的证据顶替）：

- **Linux systemd 按点运行**：没有 Linux 目标机，仍未执行；macOS 的三轮结果不替代该平台验收。
- **正式目录的长期自动清理部署**：本次只验收独立五文件 scope，测试调度已经卸载，没有安装正式清理任务。
- **到期后的永久清空**：30 天保留期，短期试运行碰不到；只有受控时钟的用例覆盖。
- **大目录、多 scope、真实增量压力**：本次只有 5 个文件，吞吐结论仍以历史性能记录为参考，不外推。
