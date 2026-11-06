# 目标服务器验收：linux-amd64 制品、真实双模型混合批次与 systemd 真实触发

日期：2026-11-06（Asia/Shanghai）
主机：阿里云 ECS，Alibaba Cloud Linux 3（OpenAnolis Edition），kernel `5.10.134-19.8.al8.x86_64`，2 vCPU，
根卷 `ext4`（`/dev/vda3`，40 G），systemd 239，SELinux disabled
制品：`dustkeeper-1.0.0-phase1-linux-amd64`，commit `e78057e`，CI run `42057572813`
（[两平台 job 均成功](https://github.com/litany-lty/dustkeeper-cleaner/actions/runs/42057572813)）

本文补的是上一轮明确留白的部分：上一轮是 macOS 本机代跑（[server-acceptance-local-20261106](server-acceptance-local-20261106.md)），
模型是本地假端点。这一轮换到真正的 Linux 目标机、真正的 ext4、真正的 systemd 用户级 timer，
模型是**真实的 JEV + MiniMax**。

## 1. 安装与制品核对

目标账号是新加的非特权账号 `dkacct`（uid 1000，`/home/dkacct`）；验收目录
`/home/dkacct/dk-acceptance-20261106`（0700，属主 `dkacct`），与机器上任何既有服务无关。

```sh
sha256sum -c SHA256SUMS            # dustkeeper-1.0.0-phase1-linux-amd64: OK
grep -E '^(commit|sha256|target)=' release-manifest.txt
install -m 0755 dustkeeper-1.0.0-phase1-linux-amd64 /home/dkacct/dk-acceptance-20261106/bin-dustkeeper
bin-dustkeeper version
```

| 核对项 | 结果 |
|---|---|
| `sha256sum -c` | OK |
| manifest `sha256` | `f1b0735a4396fe1a091a89d0e2983423c3a839bda039b6ca1d9b3aed4cc37000` |
| manifest `commit` / `target` | `e78057e` / `linux-amd64` |
| `file` | `ELF 64-bit LSB executable, x86-64, statically linked, stripped` |
| `version` | `dustkeeper 1.0.0-phase1 (linux/amd64, SQLite 3.53.4, commit e78057e)` |

前置条件逐项落地（第 1 节表格）：

- `lsof` 目标机**原来没有**，`dnf install -y lsof` 装了 4.93.2。程序按 `/usr/sbin`→`/usr/bin`→`/sbin`→`/bin`
  找到 `/usr/bin/lsof`。这是服务器部署的必装项，不是可选项。
- 内核 5.10，满足 `openat2` 要求。
- 用 `dkacct` 跑，不用 root 去清理目录。

## 2. 能力探测与只读检查

`doctor --scope mixed`（独立状态目录 `real/state`、配置 `real/config`）：

```text
ok   state_dir                expected a private state directory at .../real/state
ok   database                 1 scopes, 0 rules, 0 items
ok   capabilities_state_dir   exclusive rename, directory sync, no-follow traversal and full device barrier verified
ok   pause_marker             running
ok   pending_operations       0 operation(s) need reconciliation
ok   clock                    no previous run recorded
ok   scope mixed              active
```

也就是说 ext4 上排他 rename、目录 fsync、逐段不跟随、完整设备屏障这四项探测全部通过，与 APFS 本机的结论一致。
`scan --scope mixed`：`1 directories, 4 files (69 bytes)`，`incomplete=false`。

`doctor` 全绿**不包括**模型链路（第 5 节），所以模型那一半由第 3、4 节的真实 `run` 证明。

## 3. 真实双模型配置

`real/config/config.json` 的 `review` 段（凭据在 `real/creds/credentials.json`，`0600`，路径全程无符号链接）：

| 项 | 值 |
|---|---|
| `jev_url` / `jev_model` | `https://api.typesafe.ai/v1/systemone` / `jev-1.14.0` |
| `minimax_url` / `minimax_model` | `https://api.minimaxi.com/anthropic/v1/messages` / `MiniMax-M3.1-Flash` |
| `jev_proxy_url` | `http://127.0.0.1:18991`（该机已有的 mihomo） |
| `jev_min_confidence_pct` / `jev_min_allow_probability_pct` | `70` / `80`（第 8 节的校准闸门） |
| `max_candidates` / `timeout_seconds` | `5` / `60` |

目标机的网络事实（直接影响部署）：

- `https://api.typesafe.ai/` **直连返回 451**（地域策略）。经 `127.0.0.1:18991` 代理可达。
  所以 JEV 那一路必须显式配代理，程序不做直连回退。
- `https://api.minimaxi.com/` 直连可达（根路径 404 属正常）。
- MiniMax 不走 JEV 的代理，这是既有设计，本轮复现了它：两条链路出口不同。

闸门取 70/80 是**校准值**，不是放松：默认 90/90 下按历史实测（置信度最高 0.79、允许概率最高 0.86）
一条都过不去，删除路径永远不可达。这一轮要验证的正是"两方都同意即删除"这条路，所以显式写下校准值，
并把它记在这里。

配置写完先做只读的模型检查（不动文件，也不改生产状态），这是唯一能在 `run` 之前证明"两条链路都通"的方式
（`doctor` 不含这项）：

```text
debug: 0 simulated cleanup(s), 4 model request(s)
actual deletion: 0 files / 0 bytes; production state unchanged
  "declined.zip": advisory_only (model opinion recorded; …; file retained)
  "observable.zip": advisory_only (model opinion recorded; …; file retained)
  "recent.tmp": keep (local protection or recent activity)
```

2 个候选 × 2 层 = 4 次真实请求，`EXIT=0`。上一轮在本机用假端点跑同一命令得到 `MODEL_UNAVAILABLE`
（假端点只答 triage 那一套，advisory 的问答形状对不上），所以这条命令**只在真实端点上才算通过**——
这一轮通过了。

**本轮真实调用量**（按候选计，不是按轮计）：第 4 节第一次 `run` 3 个候选 = 3 次 JEV + 1 次 MiniMax；
systemd 触发的那次 2 个候选 = 2 次 JEV（两个都被 JEV 判 `insufficient`，没有第二层）；
只读检查 2 个候选 × 2 层 = 4 次。合计约 **8 次 JEV + 2 次 MiniMax**。这是要记账的部分。

## 4. 混合批次（真实模型，一次 `run`）

四个合成文件（三个 mtime 固定为 2020-01-01）：

| 文件 | 类别 | 期望 |
|---|---|---|
| `removable.tmp` | `temporary_name_only`，在策略直删列表里 | 双模型同意则直接删除 |
| `observable.zip` | `archive_unknown_contents` | 同意则进观察区；不同意则保留 |
| `declined.zip` | 同上 | 保留、字节不变 |
| `recent.tmp` | 未满 24 小时门槛 | 根本不进计划 |

确认策略 `cleanup setup`（两段，digest `sha256:6e11186b…`）：`--scopes mixed --categories temporary_name_only
--observation-days 7 --max-files 5 --max-bytes 1073741824 --notify none --keep-retry-days 0`。
规则 `*.tmp` / `*.zip`，`min_mtime_age_seconds=86400`，`retention_seconds=604800`；
`rule preview` 选中 3 个（`recent.tmp` 如预期不在内），`rule activate` 后
`rule enable --mode cleanup` 写了两条授权：`grant_ebb84079c0b553f29aa9528ef80a9d96`（隔离）、
`grant_f47eeddaf8f20bacd6d7ed28d0587f08`（清空），各 100 文件 / 1 GiB。

一次真实 `run`（`run_42adc3cadbeab8cc1640299953dd77d3`，14:59:12–14:59:21，exit 0）：

```text
quarantined 0 object(s), purged 1 object(s), read 36 bytes
this run: 1 deleted, 0 moved to the observation area, 2 left alone
mixed   scanned=4 complete=true quarantined=0 purged=1
    notifications are off: notifications are turned off in the confirmed cleanup policy
```

结果页（`real/out/latest.md`，页面与 `latest.json` 同一份汇总）：

```text
已删除（1）removable.tmp   18 B   双模型同意永久删除
待删除（0）本次没有文件进入待删除区。
保留（2）  declined.zip   17 B   无法确认移动是否安全
          observable.zip  19 B   无法确认移动是否安全
```

`latest.json` 里 `reason` 是代码给出的结论码，不是文案：

- `removable.tmp` → `dual_review_agrees`（两份模型意见都同意才写这个码），
  事件里的完成记录是 `path_removed=true`、`captured_then_unlinked=true`、
  `content_hash=sha256:0752436a1c9f3322e23a19683338a54ced955f79e7a5ba732cc5e4e442b1c308`；
- `declined.zip`、`observable.zip` → `move_safety_unknown`，决策记录 `verdict=insufficient`
  （JEV 主动判"依据不足"），所以**第二次模型没有被调用**——这是双模型门禁的顺序，不是漏问。

目录回读：`removable.tmp` 已不存在；`declined.zip`（17 B）、`observable.zip`（19 B）字节未变；
`recent.tmp` 仍在；`quarantine list --scope mixed` → `no objects in the vault`。

这一轮真实模型的分布与本机假端点不同，值得记下来：真实 JEV 对两个压缩包都给 `insufficient`
（不确认移动安全），对 `temporary_name_only` 的那个给了 allow，MiniMax 在同一文件上同意——
**真实链路只在"名字就是临时文件"这一类别上走通了删除**。这不是产品行为差异，是模型判断差异。

## 5. systemd 真实触发一次

`schedule render --platform linux --output real/schedule --label dustkeeper-real` 生成
`dustkeeper-real.service` / `dustkeeper-real.timer` / `README.txt`；模板形状与文档一致：
`Type=oneshot`、`ExecStart=… run`、两条 `Environment=`（`DUSTKEEPER_STATE_DIR` / `DUSTKEEPER_CONFIG_DIR`）、
`UMask=0077`、`NoNewPrivileges=true`、`Nice=10`、`IOSchedulingClass=idle`、`TimeoutStartSec=infinity`、
`OnCalendar=*-*-* 03:30:00`、`RandomizedDelaySec=10m`、`Persistent=true`。

安装与启用（`dkacct`，用户级）：

```sh
loginctl enable-linger dkacct          # 服务器无会话，需要这一条才会在没有登录时也跑
cp dustkeeper-real.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now dustkeeper-real.timer
```

`enable` 后 `list-timers` 显示 `Sat 2026-11-07 03:32:38 CST`（03:30 + 随机延迟），说明单元被真正接管。

为了在验收窗口里看到一次触发，加了一个**仅用于验收**的 drop-in
`dustkeeper-real.timer.d/acceptance.conf`（`OnCalendar=*:0/2`、`RandomizedDelaySec=0`、`Persistent=false`），
`daemon-reload` + `restart` 后观察到：

```text
15:02:13 systemd[655934]: Starting Dustkeeper bounded cleanup...
15:02:34 systemd[655934]: Started Dustkeeper bounded cleanup.
Process: … ExecStart=/home/dkacct/dk-acceptance-20261106/bin-dustkeeper run (code=exited, status=0/SUCCESS)
```

**这一轮 `run` 是 systemd 自己叫起来的，不是我在终端里敲的。** 它跑出
`run_982a8adbe2a36a5b8068fb5dd3b1bf8f`：14:59 那次之后剩下的两个文件这次都被保留
（`verdict=insufficient`），`已删除（0）`、`待删除（0）`、`保留（2）`，21 秒完成——真实网络下
每个候选一次 JEV 调用就是这个量级。

验收完删掉 drop-in 并 `restart`，`list-timers` 回到 `Sat 2026-11-07 03:35:19 CST`，即模板原本的
每日 03:30（含随机延迟）。**没有改动模板本身，也没有把定时改成长期两分钟。**

收尾时把这个 timer 停用并把 linger 关掉，避免一台验收机器每天真实调用两次模型：
`systemctl --user disable --now dustkeeper-real.timer`（`is-enabled=disabled`、`is-active=inactive`）、
`loginctl disable-linger dkacct`（`Linger=no`）。单元文件、状态目录与结果页都保留，重新启用只需要
`enable-linger` + `enable --now`。

## 6. 与停止条件的对照

| 项目 | 这一轮的状态 |
|---|---|
| linux-amd64 制品与 manifest/摘要对应 | 通过（`f1b0735a…` / `e78057e` / `v1.0.0-phase1` 三者一致） |
| 目标账号、独立目录、ext4 能力探测 | 通过（`dkacct`，四项能力全绿） |
| 真实 JEV + MiniMax 端到端 | **通过**：一次真实 `run` 里 JEV `allow` → MiniMax 同意 → 永久删除；另两个候选 JEV `insufficient` 后未进第二层 |
| 只读模型检查（`debug --root --execute --advisory`） | **通过**：4 次真实请求，`EXIT=0`，文件与生产状态未变 |
| 混合批次三类收敛 | **部分**：删除 1、保留 2、观察区 0、未进计划 1。观察区这条腿在真实模型下没有出现（模型不确认压缩包移动安全），ext4 上的隔离/恢复路径本轮未走 |
| systemd 真实触发 | **通过**：timer 触发 `dustkeeper-real.service`，exit 0，写出结果页 |
| 主动通知 | 明确选 `none`；事件里 `channel=none`、`delivered=false`、`attempted=false`，运行摘要写"notifications are off"，没有把它记成故障 |
| F1（恢复后授权失效） | 决策为"保留"，写入 [运维手册 9.1.1](../operations.md)；本轮没有产生隔离对象，未在 Linux 上复现 |

观察区那条腿为什么没走：`archive_unknown_contents` 需要 JEV 明确 allow 才可能进入观察区，
真实 JEV 对这两个包都给了 `insufficient`。这不是本轮缺步骤，而是真实模型的判断。
"同一套混合批次产生 deleted=1 / observed=1 / kept=1" 的证据目前有两处：本机 macOS（APFS，本地假端点）
和发布流水线的冒烟（`release-manifest.txt` 的 `mixed_batch="deleted=1 observed=1 kept=1"`，runner 是
ubuntu-latest）。**这台 ext4 主机上没有 observed=1**，要用真实模型在它上面凑出这条腿，等于让模型改判断，
本轮不做。要说清这一点，不能拿别处的 observed=1 替这台机器作证。

## 7. 本轮记录到的部署事实（不是缺陷）

1. **`lsof` 必须装**：目标机初始没有，缺了它自动隔离/清空不会发生，结果是 `KEPT / facts_missing`。
2. **JEV 直连 451**：目标机需要自己的 HTTP CONNECT 出口，填 `review.jev_proxy_url`；没有出口时整轮
   退化为模型不可用，不动文件。
3. **系统 `sqlite3` 太旧**：机器自带 3.26，直接打开 `state.db` 报 `malformed database schema (meta) -
   near "STRICT": syntax error`。程序内嵌 3.53.4 不受影响；人工排查用 `status` / `events` / `cleanup report`，
   不要用旧客户端改库。已写入运维手册故障速查。
4. **用户级 timer 需要 linger**：服务器没有登录会话，不 `enable-linger` 的话单元随最后一个会话结束而停。
   `README.txt` 里已经提示这是运维决定，本轮照做了一次。

## 8. 复现步骤（本机 → 目标机）

```sh
gh run download 42057572813 -n dustkeeper-linux-amd64 -D /tmp/dk-art-linux
scp /tmp/dk-art-linux/* elidli:/home/dkacct/dk-acceptance-20261106/install/
# 目标机上（dkacct）：
sha256sum -c SHA256SUMS && install -m 0755 dustkeeper-1.0.0-phase1-linux-amd64 ../bin-dustkeeper
export DUSTKEEPER_STATE_DIR=…/real/state DUSTKEEPER_CONFIG_DIR=…/real/config
bin-dustkeeper init
bin-dustkeeper scope add --path …/real/tree --kind managed_disposable --name mixed
bin-dustkeeper doctor --scope mixed && bin-dustkeeper scan --scope mixed
bin-dustkeeper cleanup setup …            # 两段：先看摘要，再 --digest
bin-dustkeeper rule import --file rule.json --id mixed-rule
bin-dustkeeper rule preview --id mixed-rule --revision 1     # 拿 review digest
bin-dustkeeper rule activate --id mixed-rule --revision 1 --digest sha256:…
bin-dustkeeper rule enable --id mixed-rule --revision 1 --mode cleanup   # 再带 enable digest
bin-dustkeeper run
bin-dustkeeper cleanup report
bin-dustkeeper schedule render --platform linux --output …/real/schedule
```

验收目录保留在目标机 `/home/dkacct/dk-acceptance-20261106`（含状态库、结果页、两份 unit 与渲染出的模板），
作为证据；没有删除、也没有把它接到任何生产目录上。

## 9. 结论

`e78057e` 的 linux-amd64 制品在一台真实的 Linux 服务器上，以非特权账号、ext4、真实 JEV + MiniMax、
systemd 用户级 timer 跑通了：缺审核不动文件、两方同意才删除、systemd 能把它叫起来并且 exit 0、结果页可查。
**这台机器上未得到证据的两项**是：观察区（`observed=1`）在真实模型下的那一条腿，以及正式通知送达
（明确选 `none`）。

正式实例仍然暂停，`~/.local/bin/dustkeeper` 上的旧版本（`2cbf8cb`）没有被替换，本机也没有重启任何调度；
目标机上的验收 timer 已停用，不会每天自己跑。
