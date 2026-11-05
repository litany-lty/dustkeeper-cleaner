# 双模型建议评测入口

`cmd/review-eval` 是正式启用前的 P0 小样本评测工具。它只把人工标注的**合成** `review.Candidate` 交给现有 JEV + MiniMax `Judge`，输出建议和汇总；它不打开 Dustkeeper 状态库、不扫描目录，也没有隔离或删除文件的能力。

## 安全边界

- 默认是 dry-run：只读取公开配置和合成样本，打印脱敏计划；不读取凭据、不建立 Judge、不发模型请求。
- 只有显式加入 `--execute` 才读取 `review.credentials_file` 并调用模型。
- `--max-cases` 必填，范围 1–6。现有适配器每个样本先调用一次 JEV，只有通过才调用一次 MiniMax，因此最多 12 次请求；无重试。
- MiniMax 的请求上限是 4096 个输出 token；thinking token 也计入该上限。M3.1 Flash 按官方兼容说明强制 thinking，本工具不发送关闭 thinking 或改变 effort 的字段，保持已配置模型的默认行为。响应允许一个或多个已知 `thinking` 块后跟恰好一个最终 `text` 块；只有最终文本中的严格 JSON 能产生决定，thinking 内容不记录、不输出也不参与判断。结果记录实际返回的 JEV/MiniMax 模型名。
- 第一项“人工标注应保留、模型却放行”的误放行，或任何模型/协议错误，立即停止后续样本。
- 输出不包含凭据、端点、查询参数或原始响应。配置仍拒绝带凭据、query 或 fragment 的端点。
- `review.Decision` 现在分别保留 JEV 和 MiniMax 返回的 token usage，缺失不会伪装为0；JSONL提供相应字段。token用量不等于人民币账单，仍需按服务商计费核对预算，不能只依赖请求次数上限。

内置样本位于 `cmd/review-eval/samples.synthetic.json`，覆盖中文可重建缓存、唯一账单、看似安装包的备份、上下文不足和诱导性文件名。样本中的 `backup_verified` 只是合成的模型输入事实，不代表工具检查过真实备份，也不能作为任何真实文件操作的证据。

外部样本同样必须包含 `"synthetic": true`，并使用 `scope_id: "synthetic"` 和 `content_sha256: "synthetic:..."`。`expected` 只能是 `allow` 或 `keep`。不要把真实文件名、用途、摘要或其它私有元数据放入样本。

## 运行

先运行零调用计划，核对样本数、模型名和最多请求数：

```sh
go run ./cmd/review-eval \
  --config /absolute/path/to/development/config.json \
  --max-cases 6
```

得到单行计划，其中应有 `"execute":false`、`"credentials_read":false`、`"max_model_calls":10`、`"max_minimax_output_tokens_per_call":4096`、`"max_minimax_output_tokens_total":20480`。这里按内置5个样本计算总量；使用其它样本或上限时，以实际选中数量为准。

获得单独的真实模型调用与费用授权后，才可显式执行并保存 JSONL：

```sh
go run ./cmd/review-eval \
  --config /absolute/path/to/development/config.json \
  --max-cases 6 \
  --execute > review-evaluation.jsonl
```

也可用 `--samples /absolute/path/to/synthetic-samples.json` 替换内置样本。每个 case 行包含人工预期、实际 `allow/keep`、单独的`judgment`、原因和实际模型名；最后一行是 summary。`judgment=insufficient`计入`abstentions`，不能被当作一次明确判断通过。退出码 `1` 表示存在不匹配、误放行或不确定，`2` 表示配置、模型或输出错误，二者都不能视为通过。

这只是模型建议质量证据，不会产生文件操作授权，也不替代现有备份、身份、保护路径、保留期、intent 事务和人工发布门禁。

## JEV诊断与显式代理

外部JEV现在要求设置 `review.jev_proxy_url`（例如本机Clash的 `http://127.0.0.1:7890`），代理失败不直连，MiniMax使用独立直连。每条case的 `jev` 包含choice、confidence、完整分布、固定0.9阈值及具体blockers。JEV usage可用时一并记录；这不代表已经提供MiniMax完整用量或实际费用计量。详见 `jev-diagnostics-20261030.md`。


外部样本必须是至多256 KiB的普通文件，在读取前和读取过程中均检查上限。plan中的`credentials_read=false`表示生成计划时尚未读取密钥，`credentials_required`表示执行所需能力；`usage_supported`/`jev_usage_supported`表示接口支持用量字段，实际是否收到用量以各条结果为准。
