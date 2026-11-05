# Debug 模拟清理

`dustkeeper debug` 对一个明确目录执行只读盘点、证据核验、JEV 判断、MiniMax 独立判断和文件复验。debug 会保留两个模型的意见，即使JEV不放行也会询问MiniMax，便于定位分歧；两者均通过才可能记录模拟清理，第二意见不能覆盖JEV拒绝。正式执行仍沿用JEV拒绝即停止的流程。它不会调用隔离/清空执行器，不打开正式数据库，不改变规则、授权或暂停状态。

默认只生成待审核清单，**不读密钥、不调用模型**。显式使用 `--execute` 才会调用现有配置的模型；这里的 execute 指执行审核，不是删除。

```sh
# 使用已有开发配置；不需要 init 或登记 scope。
export DUSTKEEPER_CONFIG_DIR=/absolute/private/development-config

/absolute/path/dustkeeper-dev debug \
  --root /absolute/directory \
  --scope-id debug \
  --max-candidates 3 \
  --json > /absolute/outside-root/debug-plan.json

# 先确认清单和外发范围，再执行真实模型审核。
/absolute/path/dustkeeper-dev debug \
  --root /absolute/directory \
  --scope-id debug \
  --evidence-file /absolute/private/evidence.json \
  --max-candidates 3 \
  --open-files \
  --execute --json > /absolute/outside-root/debug-result.json
```

`--scope-id` 是证据中对应的标识，默认 `debug`，不创建正式 scope。`--evidence-file` 只覆盖本次内存配置；证据格式沿用 [现有审核格式](safe-review.md)。不自动替用户声明文件无用或制造保留副本。缺少证据会保留并记录原因，不能为了发起模型请求跳过证据检查。

## 受控范围

- 默认最多3个待审核候选、最多6次模型请求。`--max-candidates` 允许1–6，每候选最多JEV和MiniMax各一次，无自动重试。连接或协议错误会停止本轮。
- 最多盘点500项，`--max-entries` 可调整；盘点不完整时不调用模型。
- 总审核期限120秒；单请求仍沿用20秒。默认源文件加副本核验读取预算64 MiB，纯advisory不预留不会使用的备份预算，`--max-read-bytes` 上限1 GiB。超出预算保留，不采样冒充完整内容核验。
- `--days` 默认30；近期活动、已知引用和保护类型优先保留。年龄不等于无用。可重复使用 `--reference relative/path` 标记明确引用；`--open-files` 仅能观察当前用户可见进程。
- 只发送必要相对路径、用途、处置理由、大小、摘要、核验标记与有限使用/引用事实。不发送文件正文、保留副本绝对路径或密钥。未知使用史和引用范围保持未知。
- `--action` 默认 `purge`，可选 `quarantine`；它指定模型评估的假设动作。debug 不伪造正式授权，也不宣称已验收隔离区保留期或正式到期清空。

读取可能由操作系统更新访问时间；源文件内容、修改时间、规则、数据库和真实清理计数不由 debug 修改。

## 如何复核和改进

每项报告包含扫描事实、候选类别、缺失依据、模型原始选择与分布、实际模型名、token用量（服务端提供时）、文件复验结果和停止原因。MiniMax在debug下还返回不超过400字的简短判定说明与缺失证据类别；说明不参与授权，声明缺失证据时不会模拟放行。所有模拟放行仍标记 `pending_human_review`。

1. 检查 `simulated_cleanup` 的用途、处置依据、引用覆盖和保留副本是否合理。双模型一致不是安全概率保证。
2. 对应保留却放行、应放行却保留的实际案例，保存输入事实、人工预期、实际结果，先复现，再修复对应事实采集或判定逻辑。不得通过降低阈值或把未知写成已知换取放行。
3. 重新执行同一受控案例，核对修复；避免无边界重复付费调用。

`simulated_cleaned_files` / `simulated_logical_bytes` 仅描述假设结果。`actual_deleted_files`、`actual_freed_bytes` 始终为0，`production_authorized` 始终为false；输出不进入正式审计流水。审核通过不会解除正式暂停。

2026-10-31：本地已验证默认零调用、不创建数据库、真实审核组件经本机假模型调用、源文件/数据库保持不变、已知引用优先保留、源文件变化被拒绝，以及请求次数包括失败请求且不会超过上限。真实模型验收另保存结果，不能由这些本地用例推断判断准确率。debug的网络或协议失败会同时保留脱敏后的具体错误原因。

当前真实验证复用 `jev_proxy_url=http://127.0.0.1:7890` 和 MiniMax 中国区 `https://api.minimaxi.com/anthropic/v1/messages`；不会自动启动或重配系统代理。


## 本轮真实接口修复

真实MiniMax响应在标准消息旁附带 `base_resp`。之前的严格解析器把它当作未知字段，导致成功响应也被保留。现在仅兼容这个已观察到的可选状态对象：存在时必须有整数 `status_code=0`；非零、缺失状态或异常结构均拒绝。判断阈值、JSON决定和双模型一致条件不变。[MiniMax官方客户端中的状态码处理](https://github.com/MiniMax-AI/MiniMax-MCP/blob/main/minimax_mcp/client.py)

Anthropic兼容消息与模型参数参照 [MiniMax官方消息接口](https://platform.minimax.cn/docs/api-reference/text-chat-anthropic)。本轮继续复用原有中国区地址和密钥，没有因解析失败更换密钥或自动切换代理。

同轮还修复了已观察到的 `usage.output_tokens_details` 扩展对象兼容；其子项只接受非负整数计数，核心用量字段的缺失、空值和非法类型仍拒绝。该明细只记录用量，不作为删除依据。


## Downloads 等真实目录的建议审核

`--include relative/path` 可重复指定本轮唯一允许送模型的文件，报告也仅展示选中文件。目录元数据扫描仍受 `--max-entries` 约束；扫描不完整时不发请求。

`--advisory` 用于尚未确认处置依据或保留副本的真实候选。程序核验源文件、诚实标记用途/可恢复性未知，并记录两个模型的建议；即使模型同意，也只能得到 `advisory_only`，`simulated_cleaned_files` 仍为0。此模式不产生、替代或补造正式审核许可。

```sh
dustkeeper debug --root "$HOME/Downloads"   --include 'one-reviewed-file.md'   --max-entries 30000 --max-candidates 1   --advisory --minimax-timeout-seconds 45 --execute --json
```

在发送真实文件信息前确认外发范围和服务。不会发送正文、绝对路径或密钥。若缺少这项授权，只做本地盘点。

`--minimax-timeout-seconds` 仅调整本次debug的MiniMax等待时间，范围1–60秒，默认20秒；JEV和生产默认等待不变，整轮仍最多120秒，不自动重试。`decision.request_timings` 记录每个提供商的DNS、连接、TLS、首字节和总耗时；缺失阶段保持缺失，不记作零延迟。日志不含URL、IP、请求头或响应正文。网络正常不等于模型判定已经通过。

2026-10-31 后续检查补强了凭据导出文件名的本地保护，安全策略版本升为3。现有计划与自动授权仍按版本和现场证据校验，debug不解除正式暂停。


## 用户已确认不用的副本

`--owner-confirmed-unused relative/path` 仅可与 `--advisory` 一起使用，记录所有者已明确结束该副本用途。它不会被解释成“没有程序引用”或“备份已验证”，也不会产生删除许可。没有明确确认时，不使用此参数。

网络诊断实测包含：TLS已完成但20秒内未等到响应头；同一接口也曾在约6秒完成。因而只能确认响应等待存在波动，不能认定DNS/代理故障或宣称永久修复。45秒参数仅适用于本次debug等待，正常清理仍使用原默认值。


## 30轮通用改进后的判定记录

报告同时给出`source_read_bytes`、`backup_read_bytes`和`total_read_bytes`。超预算的大文件不耗掉本轮后续小文件的审核名额；未送审的对象标为`not_reviewed`，不会混作模型判断。

用户的不用确认在本地分类和模型输入中保持一致；时间事实同时携带本次观察时点与近期窗口。扫描到审阅之间核对完整变化标记；模型等待期间扫描目录发生变化时，引用观察作废并停止本轮。引用和显式子树保护同时按文件身份识别别名。敏感目录本身也不能作为扫描根。

`--advisory`可为旧备份、日志记录只读意见；凭据、已知引用、可见占用和近期活动仍优先保留。此模式始终不生成模拟清理许可。MiniMax在debug中必须返回明确的简短说明与缺失证据清单；字段缺失、空值或协议异常立即停止。每次debug使用独立审核会话，不改变调用者的正式限额和等待设置。详细记录见[本轮30项记录](tasks/debug-thirty-general-20261031.md)。

目录本身被Finder等进程打开时，仅记录`open_directory_context`；它不证明子文件正在使用。只有观察到具体文件被打开才作为本地占用保留条件。两种信号均不证明全系统不存在其它引用。
