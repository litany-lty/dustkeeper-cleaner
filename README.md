# Dustkeeper

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/go-1.26.1-00ADD8)

一次有边界、可恢复的文件清理器。它把「策略」「授权」「执行」「恢复」拆成四件互不代劳的事：
模型只写规则草稿，程序只按已批准的规则和额度行动，任何删除都能先撤销，删不干净的状态一律停下来等人看。

一期实现是 Go + 原生 SQLite 的单个可执行文件，无常驻进程、无后台模型、不依赖 Python/Java。

> **状态：一期收口中，尚未验收。** darwin/arm64 上"注册 → 计划 → 隔离 → 不覆盖恢复 → 到期清空 →
> 重启对账"的闭环已经跑过；其余三个发布目标只交叉编译 + 产物事实检查（musl 来源、静态链接、架构），
> 未在真机执行，标为实验性。仍未关闭的项（`dedup` 峰值内存、扫描每目录提交开销、若干故障矩阵用例）
> 逐条列在 [`docs/compliance.md`](docs/compliance.md) §7.3，不用"功能结构齐全"代替验收。

> 它不猜「哪个文件你没用」。它只执行你写下来、复核过、并按版本授权过的那一条规则。

```
dustkeeper init
dustkeeper scope add --path /srv/app/disposable-exports --kind managed_disposable --name exports
dustkeeper doctor --scope exports
dustkeeper scan --scope exports
dustkeeper rule import --file rule.json --id rule-001
dustkeeper rule preview --id rule-001 --revision 1
dustkeeper rule activate --id rule-001 --revision 1 --digest <review_digest>
dustkeeper rule authorize --id rule-001 --revision 1 --auto-quarantine
dustkeeper rule authorize --id rule-001 --revision 1 --auto-quarantine --digest <approval_digest>
dustkeeper run
```

## 四个不变量

这四件事由代码结构保证，不是文档约定：

1. **注册不等于授权。** `scope add` 只登记目录。移动文件需要规则被激活**并且**单独授予 `auto_quarantine`；永久删除需要再单独授予 `auto_purge`。权限永远不追溯：先隔离、后开自动清空，旧对象仍然只能人工清空。
2. **先记账再动手。** 每次 rename/unlink 之前，先落盘 manifest（fsync 文件、fsync 目录、同卷 anchor 上做 `F_FULLFSYNC`），再提交 intent。中断之后，文件要么还在原处，要么在隔离区里且身份与内容哈希已被记录。
3. **不覆盖任何东西。** 隔离和恢复都用内核的排他 rename（Linux `renameat2(RENAME_NOREPLACE)`、macOS `renameatx_np(RENAME_EXCL)`）。没有可用的排他 rename 就硬失败，绝不退回覆盖 rename 或 copy+unlink。
4. **不确定就停。** 身份对不上、时钟异常、预算耗尽、目录被替换、keeper 消失：进入 `held` / `unknown` 并停下，绝不用猜的结论继续下一个文件。

## 命令

| 命令 | 作用 | 是否改文件 |
|---|---|---|
| `init` | 建状态目录、默认配置、数据库 | 否 |
| `doctor` | 检查权限、数据库、文件系统能力、时钟、待对账 | 仅自有探针 |
| `scope add/list/show/pause/resume` | 登记与管理范围 | 仅可变更范围创建私有隔离区 |
| `scan --scope ID` | 只读元数据扫描，建立候选索引 | 否 |
| `rule draft --text ...` | 让模型生成草稿（可离线用 `rule import`） | 否 |
| `rule import/show/validate` | 手写规则的导入、查看与校验 | 否 |
| `rule preview --id --revision` | 冻结一份**不落库**的预览计划，给出 review 摘要 | 否 |
| `rule activate --digest ...` | 固定规则版本，旧版授权作废 | 否 |
| `rule authorize/revoke/disable` | 分别管理两种后台能力 | 否 |
| `plan create/show/apply` | 冻结并执行隔离计划 | apply 会隔离 |
| `dedup plan --scope --keeper-scope` | 精确查重并生成计划 | 否 |
| `quarantine list/restore` | 查看与不覆盖地恢复隔离对象 | restore 会移动 |
| `protect add/list/remove` | 管理路径保护（不自动过期） | 否 |
| `purge plan/show/apply` | 到期清空的独立计划与执行 | apply 永久删除 |
| `recover inspect/reconcile` | 读取未完成 intent，按矩阵对账 | reconcile 可能补偿恢复 |
| `status` / `events` | 运行状态与审计 | 否 |
| `run` | 对账 → 到期清空 → 扫描 → 已授权隔离 | 取决于授权 |
| `pause` / `resume` | 全局暂停标记 / 显式解除 | 否 |
| `schedule render` | 生成 systemd / launchd 模板 | 否 |

所有命令支持 `--json`，输出包含 `schema_version`、`status`、`code`、`message`、`retryable`、`run_id`，以及相关的 scope / plan / object ID。计划类结果另带 `planned_count` / `completed_count` / `skipped_count` / `failed_count`。

退出码：`0` 成功（含「无候选」）、`2` 参数或 DSL 不合法、`3` 已有任务在跑、`4` 计划过期或身份变化、`5` 能力/权限不支持、`6` 需对账或安全暂停、`7` 存储或同步失败、`8` 模型不可用、`9` 计划只完成一部分、`130` 被信号中断。

## 规则 DSL

一期每条规则只对应一个 scope，条件全部成立才匹配；排除条件优先于匹配条件。

```json
{
  "dsl_version": 1,
  "scope_id": "exports",
  "action": "quarantine",
  "match": {
    "min_mtime_age_seconds": 2592000,
    "min_size_bytes": 0,
    "max_size_bytes": null,
    "relative_dir_prefixes": [],
    "basename_patterns": ["*.zip"]
  },
  "except": {
    "relative_dir_prefixes": ["keep/"],
    "basename_patterns": ["*发票*"]
  },
  "retention_seconds": 2592000
}
```

含义：「exports 中至少 30 天未修改的 zip 文件，排除 `keep/` 子目录与文件名含「发票」的文件，隔离后至少保留 30 天」。

规则字段没有一处能表达「授权」。`retention_seconds` 是最短保留期，不是删除许可；`action` 只能是 `quarantine`。

## 保留期与清空

- 生命周期对象的保留期只来自被批准的 DSL；精确重复对象使用宿主固定的 30 天，显示在计划里并计入摘要。
- `purge_not_before = quarantined_at + retention`，到期时刻永远不会被倒推。
- 到期只是「可以生成清空计划」，不是删除。`purge plan` 生成独立计划与摘要，`purge apply --digest` 才真正 unlink，且只 unlink 隔离区里生成的名字，原路径从不交给 unlink。
- 清空前重新核验：授权仍有效、规则版本未变、对象身份与内容哈希未变、keeper 仍存活。

## 文档

- [`docs/operations.md`](docs/operations.md)：安装、定时任务、恢复流程、故障处理、备份与卸载。
- [`docs/architecture.md`](docs/architecture.md)：模块边界、状态机、对账矩阵、跨平台文件操作。
- [`docs/compliance.md`](docs/compliance.md)：与一期方案文档的逐条核对、仍需补齐的验收用例。
- [`packaging/`](packaging/)：`install.sh` / `uninstall.sh`，在目标机器上渲染并安装定时任务模板。

## 构建与测试

```sh
go build ./cmd/dustkeeper
go test ./...             # 包内测试 + 集成测试；test/eval 默认跳过
go test -race ./...
./build/build.sh          # 开发构建：能建的都建，建不了的跳过并说明
./build/build.sh --release # 发布构建：缺一个目标就失败
```

`build.sh` 有两档，因为"安静地跳过"和"照样发布"不能是同一个结果：

- **开发**：缺交叉 C 编译器时跳过该目标，并打印需要设的变量名。
- **发布**（`--release` 或 `RELEASE=1`）：`darwin-arm64`、`darwin-amd64`、`linux-amd64`、`linux-arm64` 四个目标必须全部产出，每个产物必须非空、在 macOS 上签名可验、被 `SHA256SUMS` 覆盖并且该文件自身能通过 `-c` 校验。签名失败、校验工具失败、目标缺失、产物为空，任一都退出 1。校验文件只列本轮产出的文件，`dist/` 里的旧产物不会混进来。

二进制通过 cgo 链接 SQLite，交叉编译需要目标平台的 C 编译器（`CC_<os>_<arch>`）。**Linux 的 libc 策略是固定的**：静态链接 musl，`CC_linux_*` 指向 `x86_64-linux-musl-gcc` / `aarch64-linux-musl-gcc`；不产出 glibc 版本，所以每个发布只有一种 libc 说法。Windows 不是目标：本实现依赖 `flock`、`renameatx_np`/`renameat2`、`F_FULLFSYNC` 这些 unix 原语。

### 平台支持

| 平台 | 构建 | 测试 | 判定 |
|---|---|---|---|
| darwin/arm64 | 是 | 全量 `go test ./...` + 端到端闭环 | 受支持 |
| darwin/amd64 (`CC_darwin_amd64`) | 是 | 未在目标机器上跑过 | **实验性** |
| linux/amd64 (`CC_linux_amd64`) | x86_64 宿主上是（musl 静态） | 未在目标机器上跑过 | **实验性** |
| linux/arm64 (`CC_linux_arm64`) | 本机容器内实测是（musl 静态，`smoke=ok`） | 全量 `go test ./...` 在 linux 容器（ext4 loop）通过 | **实验性** |

"实验性"是准确说法：这些目标能编译、能通过 `SHA256SUMS` 校验，但没有在真机上跑过"注册 → 计划 → 隔离 → 不覆盖恢复 → 到期清空 → 重启对账"。验收记录见 `docs/compliance.md`。

```sh
# 在容器里真实构建 linux 目标，并证明 musl 来源 + 静态链接 + 本机架构冒烟
./build/build-linux-release.sh          # 默认只构建容器自身架构
./build/build-linux-release.sh arm64    # 显式指定
```

`musl-gcc` 只为它自己被编译的那个架构工作，所以 arm64 宿主上默认只构建 `linux/arm64`；
要产出 `linux/amd64` 需要一台 x86_64 宿主（或交叉 musl 工具链）。脚本不会假装跨架构成功。

性能证据（会真的创建文件，默认不跑）。它先核对"活儿干完了"，再报资源：

```sh
DUSTKEEPER_BENCH=1 DUSTKEEPER_BENCH_FILES=100000 \
  DUSTKEEPER_BENCH_OUT=docs/evidence/performance-100k.md \
  go test ./test/bench -run 'TestSmallTreeScan|TestLargeTreeScanEvidence' -count=1 -v
```

实测（darwin/arm64，APFS，10 万文件／1 000 个叶子目录，见
[`docs/evidence/performance-100k.md`](docs/evidence/performance-100k.md)）：

| 阶段 | 结果 |
|---|---|
| 完整扫描 | 100 000 文件 / 1 051 目录，wall 44.8 s，epoch 完整，队列 pending 0，observation 100 000 行 |
| 峰值 RSS | 30.0 MiB（门槛 256 MiB） |
| 数据库 | 180 KiB → 59.0 MiB，约 619 B/文件 |
| 默认预算下的一轮运行 | 100 000 文件，扫描完整、未被预算中断，control=continue |
| 中断续跑 | 8 批，累计扫描 1.00× 树大小（没有重复读），最长单批 5.2 s |

不设 `DUSTKEEPER_BENCH_OUT` 时只打印数字、不落盘；`DUSTKEEPER_BENCH_MAX_RSS_MB` 改扫描门槛。
证据里 `content_bytes_read` 是 null（扫描不打开文件内容，没有可测值）、WAL 字节不适用
（存储固定 `journal_mode=DELETE`）、`Rusage inblock` 只作为块层提示而非精确物理读取。

fuzz 目标：`FuzzMatchName`、`FuzzHasDirPrefix`、`FuzzSplitRelative`、`FuzzValidateName`、
`FuzzParseDraft`、`FuzzParseRule`。种子每次 `go test` 都跑；深挖用 `-fuzz`。

### 真实模型评测（不进单测）

`internal/rules` 的中文语料测试只证明"这些答案能被 DSL 表达"，它不调用模型。语义那一半在 `test/eval`：

```sh
DUSTKEEPER_EVAL_BASE_URL=https://host/v1/chat/completions \
DUSTKEEPER_EVAL_MODEL=some-model \
DUSTKEEPER_EVAL_API_KEY=... \
DUSTKEEPER_EVAL_OUT=/tmp/eval.jsonl \
go test ./test/eval -v -count=1
```

每条语料写成一行 JSON：provider、model、prompt digest、输入、原始输出、宿主决定、是否与预期一致。没有 `DUSTKEEPER_EVAL_BASE_URL` 时它 `t.Skip`，所以网络不是 `go test ./...` 的前提。

## 协议

[MIT](LICENSE)。可以商用、可以修改、可以再发布，保留版权声明即可。

这个程序会移动和删除文件。协议里的免责条款不是套话：先用 `managed_disposable` 目录和 `--dry-run`
验证它在你机器上的行为，再交给定时任务。
