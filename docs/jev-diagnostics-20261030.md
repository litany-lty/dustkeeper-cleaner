# JEV 分布诊断与轻量网络代理

2026-10-30，Asia/Shanghai。本轮未部署、未提交、未解除正式清理暂停。

## 真实定向结果

沿用上轮冻结的 `confirmed-disposable-copy` 合成正例、同一模型和提示，仅增加诊断记录及显式代理。样本文件SHA-256：`e06827ae2f6d5d1a072662aabb5607a67854f692195534cd98c796f79f2f4925`。

| 字段 | 实际值 |
|---|---|
| 实际模型 | jev-1.14.0 |
| 原始选项 | insufficient |
| confidence | 0.66 |
| P(allow_review) | 0.14 |
| P(insufficient) | 0.77 |
| P(keep) | 0.09 |
| 本地拦截原因 | model_insufficient |
| 路由 | explicit_http_proxy，本机现有Clash 127.0.0.1:7890 |
| 用量 | 输入706 / 输出51 tokens |

**本次是模型主动判定信息不足，不是模型选择了允许而仅被0.9阈值挡下。** 两项阈值仍为0.9，既有“只有allow_review才可能进入MiniMax”的条件未变。更低阈值也不能改变本次原始选项；没有因此调整阈值。

本次响应没有自然语言解释，因此无法证实模型具体认为缺哪一项事实。置信度不是安全删除概率。本次结果也不能倒推前两次同样显示insufficient的响应一定具有相同分布——当时没有保存这些字段。

正例预设了“可丢弃”的事实，但模型没有接受其充分性。下一步应检查输入证据的来源、结构和可信边界，区分系统已验证的事实与文件名/自由文本声称，不用重新措辞或降低阈值追求放行。

## 实现变化

- `Decision.jev` 保留经解析校验的原始choice、confidence、完整概率分布、两个阈值、是否通过、拦截原因和JEV token用量。
- 拦截原因区分 `model_keep`、`model_insufficient`、`confidence_below_threshold`、`probability_below_threshold`。两个阈值同时不满足时同时记录。
- 评测JSONL输出完整诊断；允许动作的既有审计也随Decision包含此诊断。不输出API Key或原始自由文本响应。
- 用Go标准库在程序内封装显式HTTP(S)代理，仅JEV采用这条路由。MiniMax独立直连，不继承终端/全局代理变量。
- 外部JEV缺少代理配置时拒绝；代理不可用时返回错误，不回退直连。HTTP CONNECT代理下仍校验目标HTTPS证书。
- 导入脚本新增 `--jev-proxy-url`，默认 `http://127.0.0.1:7890`，写入开发配置的 `review.jev_proxy_url`。

## 部署使用

用户最终选择轻量网络代理方案，不再随包捆绑Clash/mihomo本体。没有下载代理内核二进制、安装全局代理或启动新的常驻代理服务，也没有读取/导出本机订阅节点。

同一程序可在Mac或服务器上使用：

```json
{
  "review": {
    "jev_proxy_url": "http://127.0.0.1:7890"
  }
}
```

以上只展示新增字段，应合并进已有review对象，不能覆盖其它模型、凭据及证据配置。服务器上填写服务器实际可用的HTTP(S)代理地址；不要求Clash桌面客户端，也不依赖shell别名。代理可以是已有Clash/Mihomo或其它提供HTTP CONNECT的服务。

**程序包含代理客户端封装，不会凭空提供代理出口。** 如果目标服务器没有可用代理，必须先配置该服务。当前代理URL不接受用户名密码、query或路径；不把access-bearing URL写入普通配置或日志。有代理认证需求时应另行设计私有凭据读取。

示例离线导入（输出目录必须是新目录，凭据不显示）：

```sh
python3 scripts/import-review-config.py \
  --minimax-mcp-config "$HOME/.claude.json" \
  --jev-proxy-url http://127.0.0.1:7890 \
  --output-dir /absolute/new/private-development-directory
```

不自动切换服务器路由、不修改全局网络设置。服务器实际网络和Linux运行尚未在本轮验证。

## 验证范围

本地假服务用例已验证：模型不足与两种阈值不足分别记录；JEV确实走指定代理、MiniMax不走该代理；代理关闭后不直连；导入配置包含显式代理。`internal/review`、`cmd/review-eval`定向包测试及导入脚本6项测试通过。

随后真实使用同一正例，通过Clash代理取得上表响应，没有模型异常，也没有放行，因此未调用MiniMax。没有为补日志再次重跑；没有运行全量发布或服务器验收。

## 累计额度和最终状态

前序10次请求；本轮JEV 1次，MiniMax 0次。**累计11/12次，剩余1次未使用**。70元总预算未重置；未读取供应商账单，不虚报实际人民币费用。

正式实例继续暂停，无待处理操作，原有6个隔离对象不动。真实双模型连续放行路径仍未验收，不能开启正式自动清理。
