# 目标机器上的有限部署验收（2026-10-30，macOS 26.5.2 / arm64）

一次真实闭环记录，不是测试报告。范围固定为：**一个专门用来丢东西的小目录**，用最终制品跑完
扫描 → 批准 → 隔离 → 查看 → 恢复 → 再跑两轮。长保留期之后的真实永久删除**没有**在这次里发生，
也不声称发生过。

## 1. 用的东西

| 项目 | 值 |
|---|---|
| 主机 | macOS 26.5.2（build 25F84），arm64；APFS，`/tmp` 在数据卷上，可用 67 GiB |
| 制品 | `dustkeeper-1.0.0-phase1-darwin-arm64`，`commit=fca60b8`，`built=2026-10-30T07:45:38Z` |
| checksum | `sha256=303ad908372c07c114fb302a5ee4ccb1e95c7256c4e09bd55c22d1c42addd143`（`shasum -a 256 -c SHA256SUMS` 通过） |
| 构建方式 | `./build/build.sh --release`（本机原生编译，含 `smoke_qr` 与发布清单校验） |
| 状态/配置 | `/tmp/dk-accept-20261030/{state,config}`，与真实安装完全分离 |
| 测试目录 | `/tmp/dk-accept-20261030/tree`，5 个文件 / 2 个目录，共 44 KiB |

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
init                     code=OK  instance_id=inst_69c0b721…  schema=3
scope add                code=OK  scope_id=accept  kind=managed_disposable  vault=true
doctor --scope accept    code=OK  六项检查全 True（state_dir / database / capabilities /
                                  pause_marker / pending_operations / clock）
scan --scope accept      code=OK  dirs=2 files=5 bytes=71 incomplete=false failed_dirs=0
rule import              code=OK  digest=sha256:fb79e41c…
rule enable（提案）       code=OK  mode=quarantine  matched=1  enable_digest=sha256:d47074e1…
                                  候选只有 old-match.zip（truncated=false）
rule enable --digest     code=OK  grant=auto_quarantine  max_files=100  max_bytes=1 GiB
run                      code=OK  scanned=5  quarantined=1  quarantined_logical_bytes=18
                                  control=continue  read_bytes=36  clock_trusted=true
status                   code=OK  observation_count=5  quarantined_count=1  due_for_purge_count=0
quarantine list          count=1  old-match.zip  due=false  auto_purge_granted=false  restorable=true
quarantine restore       code=OK  restored=old-match.zip  protected=true
run（恢复后第 1 轮）      code=OK  scanned=5  quarantined=0  purged=0
run（恢复后第 2 轮）      code=OK  scanned=5  quarantined=0  purged=0
```

隔离区 `objects/` 里是 `obj_9ef42edad5c76b169c7940a4dc1060db`（18 B），恢复之后该目录只剩
`.dk-vault-nonce`。恢复时返回 `protected: true`，随后两轮都是 `quarantined=0`：**刚恢复的文件没有被
下一轮立刻再挑中**，这是既有行为，不是这次改出来的。

`cmp` 逐字节比对恢复后的 `old-match.zip` 与原内容：相同。

## 3. 资源与体积（同一台机器，单 scope，5 个文件）

| 指标 | 实测 |
|---|---|
| `run` 墙钟 | 0.24 s（`/usr/bin/time -l`，已隔离过的稳定态；首轮含隔离写入略高） |
| 峰值 RSS | 15 597 568 B ≈ 14.9 MiB（`maximum resident set size`） |
| `state.db` | 180 KiB（含扫描、规则、授权与事件） |
| `.dustkeeper/` | 24 KiB（`objects/` 恢复后为空） |
| 日志 | `state/logs` 8 KiB，JSONL 轮转未触发 |

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

## 5. 调度：命令与环境变量对，但**不是** launchd 启动的

`schedule render --platform darwin --hour 3 --minute 30` 生成的 plist 内容与预期一致：`ProgramArguments`
是当前二进制的绝对路径，`EnvironmentVariables` 指向本次的 state/config，`StandardOutPath` /
`StandardErrorPath` 都是 `state/run.log`，没有 `KeepAlive`。

但**这次没有做到"由实际 launchd 身份启动"**。按 `docs/operations.md` 的两条路都试过，都失败：

| 做法 | 结果 |
|---|---|
| `launchctl bootstrap gui/501 ~/Library/LaunchAgents/dustkeeper.plist` | `Bootstrap failed: 5: Input/output error` |
| `launchctl bootstrap user/501 …` | 同上 |
| `launchctl submit -l dk-accept -- …` | 无输出、`launchctl list` 里没有该作业 |

`launchctl managername` 报告 `Aqua`，所以不是"不在 GUI 会话"这种常规原因；实际原因是这个执行环境
对 `launchctl` 的 bootstrap Mach 服务没有权限。装进去的 plist **已经删掉**，`launchctl print
gui/501/dustkeeper` 仍然是 `Could not find service`，主机上没有留下任何 dustkeeper 作业。

因此这一项如实记为：**本机 launchd 调度验证未做。** plist 的字段、路径与命令是核对过的，模板生成的
逻辑有 `internal/schedule` 的用例覆盖，但那不等于"launchd 真的按点跑了一次"。

systemd 一侧同样未做：本次没有 Linux 目标机。`linux-amd64` 的制品由 CI 的 `ubuntu-latest` 产出并冒烟，
容器里跑的是命令序列，不是 systemd timer。

## 6. 这次验收覆盖了什么、没覆盖什么

覆盖到：

- 最终制品在真实机器上完成 init → scope add → doctor → scan → rule import → enable 提案 → 确认 → run →
  quarantine list → restore；
- 只有匹配且未被排除的文件被移动；排除样本、保护样本与不相干文件原样不动；
- 恢复后字节一致、原路径不被覆盖，且自动加了路径保护，后续两轮没有再选中它；
- `status` 的三个计数与 `quarantine list` 的 `due/auto_purge_granted` 与事实一致；
- 资源占用落在预期量级。

没覆盖到（如实记录，不用别的证据顶替）：

- **launchd / systemd 按点运行**：本环境 launchctl bootstrap 被拒，没有 Linux 目标机。
- **到期后的永久清空**：30 天保留期，短期试运行碰不到；只有受控时钟的用例覆盖。
- **大目录、多 scope、真实增量压力**：本次只有 5 个文件，吞吐结论仍以历史性能记录为参考，不外推。
