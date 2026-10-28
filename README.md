# Dustkeeper

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/go-1.26.1-00ADD8)

一次有边界、可恢复的文件清理器。它把「策略」「授权」「执行」「恢复」拆成四件互不代劳的事：
模型只写规则草稿，程序只按已批准的规则和额度行动，任何删除都能先撤销，删不干净的状态一律停下来等人看。

一期是一个 Go + 原生 SQLite 的可执行文件：无常驻进程、无后台模型、不依赖 Python/Java。

> **状态：FINAL-1.0 收口完成，`darwin-arm64` 已本机验收；`linux-amd64` 的制品在本机构建并做了 ELF 复核，
> 但它的 Q/R 冒烟需要原生 x86_64 环境（见「支持平台」），不在本机宣称通过。**
> 逐条结论见 [`docs/compliance.md`](docs/compliance.md) 的 FINAL 状态表（A01–A12）。
> 所有证据只用结果说话：命令、commit、checksum、原始输出；不用"功能结构齐全"代替验收。

```
dustkeeper init
dustkeeper scope add --path /srv/app/disposable-exports --kind managed_disposable --name exports
dustkeeper scan --scope exports
dustkeeper rule import --file rule.json --id rule-001
dustkeeper rule preview --id rule-001 --revision 1
dustkeeper rule activate --id rule-001 --revision 1 --digest <review_digest>
dustkeeper rule authorize --id rule-001 --revision 1 --auto-quarantine --digest <approval_digest>
dustkeeper run
```

## 快速开始

```sh
# 构建（本机原生，不交叉编译）
go build ./cmd/dustkeeper                 # 或者用带检查的构建入口，见「支持平台」
./build/build.sh                          # 本机开发构建：产物能跑才叫产物
./build/build.sh --release                # 发布构建：任何一项检查不通过就失败，且不留产物

# 上手：注册一个可以丢东西的目录
dustkeeper init
dustkeeper scope add --path /srv/app/disposable-exports --kind managed_disposable --name exports
dustkeeper doctor --scope exports         # 权限、数据库、文件系统能力、时钟、待对账
dustkeeper scan --scope exports           # 只读元数据扫描，建立候选索引
dustkeeper rule import --file rule.json --id rule-001
dustkeeper rule preview --id rule-001 --revision 1
dustkeeper rule activate --id rule-001 --revision 1 --digest <review_digest>
dustkeeper rule authorize --id rule-001 --revision 1 --auto-quarantine --digest <approval_digest>
dustkeeper run                            # 对账 → 到期清空 → 扫描 → 已授权隔离
```

规则文件长这样（一条规则只对应一个 scope）：

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
字段里没有一处能表达「授权」：`retention_seconds` 是最短保留期，不是删除许可；`action` 只能是 `quarantine`。

所有命令支持 `--json`，输出包含 `schema_version`、`status`、`code`、`message`、`retryable`、`run_id`，以及相关的
scope / plan / object ID。退出码：`0` 成功（含「无候选」）、`2` 参数或 DSL 不合法、`3` 已有任务在跑、
`4` 计划过期或身份变化、`5` 能力/权限不支持、`6` 需对账或安全暂停、`7` 存储或同步失败、`8` 模型不可用、
`9` 计划只完成一部分、`130` 被信号中断。

## 日常操作

| 命令 | 作用 | 是否改文件 |
|---|---|---|
| `scan --scope ID` | 只读元数据扫描，更新候选索引与完整性 | 否 |
| `run` | 对账 → 到期清空 → 扫描 → 已授权隔离 | 取决于授权 |
| `quarantine list/restore` | 查看隔离对象；不覆盖地恢复回原路径 | restore 会移动 |
| `purge plan/show/apply` | 到期清空的独立计划与执行 | apply 永久删除 |
| `recover inspect/reconcile` | 读取未完成 intent，按对账矩阵处理 | reconcile 可能补偿恢复 |
| `status` / `events` | 运行状态与审计流水 | 否 |
| `pause` / `resume` | 全局暂停标记 / 显式解除 | 否 |
| `schedule render` | 生成 systemd / launchd 模板 | 否 |
| `protect add/list/remove` | 路径保护（不自动过期） | 否 |
| `plan create/show/apply` | 冻结并执行隔离计划 | apply 会隔离 |
| `rule draft/import/show/validate/preview/activate/authorize/revoke/disable` | 规则全生命周期，激活与授权分开 | 否 |
| `scope add/list/show/pause/resume` | 登记与管理范围 | 仅 `managed_disposable` 建私有隔离区 |
| `doctor` | 权限、数据库、文件系统能力、时钟、待对账 | 仅自有探针 |

日常节奏是「定时 `run` + 定期看 `status`/`events` + 有事才 `restore`/`recover`」。`run` 不做没被授权的动作：
隔离要 `auto_quarantine`，永久删除要再单独授予 `auto_purge`。

## 范围限制

这些是代码结构保证的边界，不是文档约定：

1. **注册不等于授权。** `scope add` 只登记目录。移动文件需要规则被激活**并且**单独授予 `auto_quarantine`；
   永久删除要再单独授予 `auto_purge`。权限不追溯：先隔离、后开自动清空，旧对象仍然只能人工清空；
   撤销、换规则版本、下调配置额度都不会扩大已隔离对象的权限，也不会让它们提前到期。
2. **只有 `managed_disposable` 会自动隔离/清空。** 其它种类的 scope 只观察，不自动动作。
3. **不覆盖任何东西。** 隔离和恢复都用内核的排他 rename（Linux `renameat2(RENAME_NOREPLACE)`、
   macOS `renameatx_np(RENAME_EXCL)`）。拿不到排他 rename 就硬失败，绝不退回覆盖 rename 或 copy+unlink。
4. **不确定就停。** 身份对不上、时钟异常、预算耗尽、目录被替换、keeper 消失、扫描不完整：进入 `held` /
   `unknown` / 不完整并停下，绝不用猜的结论继续下一个文件。扫描没读完的 scope 不会产生新的自动隔离。
5. **不越界、不动别人的文件。** 诊断日志按私有文件规范打开（不跟随 symlink、拒绝 FIFO / 非普通文件 /
   多硬链 / 过宽权限）；路径解析用 `openat2` 的 `RESOLVE_BENEATH|NO_SYMLINKS|NO_XDEV`，出界即失败。
6. **没有的东西就是没有。** 精确去重、`adopt` 支线已从生产路径移除；没有自动迁移，没有「万能恢复命令」，
   没有常驻进程和后台模型。扫描不读取文件内容（证据里 `content_bytes_read` 为 null）。
7. **Windows 不是目标**：本实现依赖 `flock`、`renameatx_np`/`renameat2`、`F_FULLFSYNC` 这些 unix 原语。

规模上限有实测：10 万普通文件、1 051 个目录，完整扫描 wall 39.6 s、峰值 RSS 30.8 MiB（既定上限 256 MiB），
数据库 180 KiB → 59.0 MiB（约 619 B/文件）；默认预算下的一轮运行扫描完整、未被预算中断（control=continue）；
中断续跑 10 批、累计只扫 1.00× 树大小。数字与测量口径见
[`docs/evidence/performance-100k.md`](docs/evidence/performance-100k.md)。

**模型能力未验收。** 自然语言 → 规则草稿这一层需要在已获准的模型配置下跑固定语料才有结论；
本仓库没有配置端点，因此 `test/eval` 保持 skip，README 不宣称自然语言能力可用。
离线规则（手写 JSON + `rule import`）是完整可用的主路径。

## 升级前旧对象处理

本期不实现自动迁移，也不以「清空旧库」「删除旧 vault」「重新初始化」来"解决"旧数据。升级按这个顺序做：

1. **先停用旧定时任务**，让旧二进制不再有新的运行。
2. **保留旧二进制和一份完整状态副本**（状态目录 + 配置目录，`cp -a` 或快照）。这份副本是后面所有判断的依据。
3. **在副本上验证，再切换。** 如果状态里有当前版本不解释的对象（旧 dedup duplicate、旧 adopt pending、
   证据不足对象），先用**与那份状态格式相符的旧版本**在副本上验证恢复流程，确认可恢复后再切换。
4. **旧版本自己也解释不了的对象，保持原数据。** 不猜、不改写、不清 pending，按 `recover inspect` 的证据人工取回；
   该 scope 的新隔离/清空会被阻断，直到这些对象被人工处理。
5. 没有一条命令能"修好一切"。健康旧对象的恢复（R）和对账收敛仍然可用，但"精简版重新验证 keeper 后删除"这种
   说法不成立：旧 duplicate 不会变成新的可清空对象。

## 支持平台

两个发布目标，别的都不叫支持：

| 目标 | 构建入口 | 本机验证到哪一步 |
|---|---|---|
| `darwin-arm64` | `./build/build.sh --release`（本机原生编译 + ad-hoc 签名） | 产物 `sha256=5f365a46…2cf0b`；`version` 与临时目录内 Q/R 冒烟在本机真实执行通过 |
| `linux-amd64` | `./build/build-linux-release.sh`（固定 digest 的 `linux/amd64` 镜像，`CC=musl-gcc`） | 制品在本机构建，`readelf` 复核 x86-64、无 PT_INTERP、无 DT_NEEDED；Q/R 冒烟需原生 x86_64 宿主，见下 |

`build.sh` 是单目标构建：它在**它所在的那台机器上**编译，产物架构从产物本身读回来，不接受 GOOS/GOARCH 覆盖。
一台不是发布目标的机器仍然能做开发构建，但产物不会被叫作受支持，也不会写发布清单。

`linux-amd64` 的制品必须在能真正执行它的环境里冒烟：本实现用 `openat2` 解析路径、用 `renameat2` 移动文件，
而在 arm64 机器上以模拟方式跑 amd64 容器时 `openat2` 返回 `ENOSYS`（Rosetta），冒烟会失败并如实报出这个原因。
因此 linux 发布件由原生 x86_64 宿主产出：那台机器自己，或 CI 的 `ubuntu-latest`（见 `.github/workflows/ci.yml`，
它在构建前先断言 runner 的实际架构就是本次声明的目标）。

构建脚本会记录它自己看到的事实：目标、Go 与 musl 版本、编译器 triple、`musl_evidence`、产物头信息、
checksum、这份制品实际跑过哪些动作。发布清单来自这些事实，不来自常量。

## 文档

- [`docs/operations.md`](docs/operations.md)：安装、定时任务、恢复流程、故障处理、备份与卸载。
- [`docs/architecture.md`](docs/architecture.md)：模块边界、状态机、对账矩阵、跨平台文件操作。
- [`docs/compliance.md`](docs/compliance.md)：FINAL 状态表（A01–A12）与证据；旧清单已标为历史。
- [`docs/evidence/`](docs/evidence/)：扫描/资源实测的原始 JSON 与摘要。
- [`packaging/`](packaging/)：`install.sh` / `uninstall.sh`，在目标机器上渲染并安装定时任务模板。

## 协议

[MIT](LICENSE)。可以商用、可以修改、可以再发布，保留版权声明即可。

这个程序会移动和删除文件。协议里的免责条款不是套话：先在一个 `managed_disposable` 小目录上跑
`scan` + `rule preview`，看清楚会动哪些文件，再交给定时任务。
