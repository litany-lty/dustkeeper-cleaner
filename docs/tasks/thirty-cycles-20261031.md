# Dustkeeper 30轮测试—修复—复测记录

日期：2026-10-31（Asia/Shanghai）。范围：本地开发副本，源码最终同步至 `/Users/litany/WorkBuddy/清理器`。

## 结果与口径

30轮均有可复现失败检查、对应代码修正及同一问题复测通过。没有将基线通过、纯阅读、重复测试、确认能力或一组测试中的多个断言算成独立一轮。相同根因的文件名/格式变体在一轮内处理。

开始时原有相关套件通过，因此新增的是原用例未覆盖的真实行为缺口；不是为了完成次数而删除保护、降低阈值或故意引入缺陷。第3轮初始测试编译错误在确认行为前修正，不作为缺陷证据。第15轮路径修复又暴露根目录遍历回归，补了正常副本的正向测试并修复，仍归入第15轮。第21轮的有效期失败证据是在排除这一回归后重新取得。

## 逐轮证据

|轮次|问题与修复主题|原检查退出码 → 复测|定向命令|
|---|---|---|---|
| 1 | 根目录保护前缀被忽略 | 1 → 0 | `go test ./internal/assess -run TestCycle01 -count=1` |
| 2 | 无效保护表达式被静默忽略 | 1 → 0 | `go test ./internal/assess -run TestCycle02 -count=1` |
| 3 | 审核接受越界和非规范相对路径 | 1 → 0 | `go test ./internal/review -run TestCycle03 -count=1` |
| 4 | 模型等待期间过期的证据仍获批 | 1 → 0 | `go test ./internal/review -run TestCycle04 -count=1` |
| 5 | 审核凭据接受早于自身创建的时钟 | 1 → 0 | `go test ./internal/review -run TestCycle05 -count=1` |
| 6 | 审核缺少根范围仍声称副本在范围之外 | 1 → 0 | `go test ./internal/review -run TestCycle06 -count=1` |
| 7 | 目录引用未传播到被引用目录内的文件 | 1 → 0 | `go test ./internal/assess -run TestCycle07 -count=1` |
| 8 | 临时目录声明掩盖安装包恢复来源缺口 | 1 → 0 | `go test ./internal/assess -run TestCycle08 -count=1` |
| 9 | 凭据和钱包文件落入空间候选 | 1 → 0 | `go test ./internal/assess -run TestCycle09 -count=1` |
| 10 | 源码类型遗漏导致误列清理候选 | 1 → 0 | `go test ./internal/assess -run TestCycle10 -count=1` |
| 11 | 文档格式遗漏丢失文档处置依据要求 | 1 → 0 | `go test ./internal/assess -run TestCycle11 -count=1` |
| 12 | 只够一次备份读取时仍花费模型调用 | 1 → 0 | `go test ./internal/review -run TestCycle12 -count=1` |
| 13 | JEV专属CA同时被MiniMax信任 | 1 → 0 | `go test ./internal/review -run TestCycle13 -count=1` |
| 14 | 无效用量记录被伪装成零用量 | 1 → 0 | `go test ./internal/review -run TestCycle14 -count=1` |
| 15 | 大小写别名绕过备份范围检查 | 1 → 0 | `go test ./internal/review -run TestCycle15 -count=1` |
| 16 | 私有文件仅检查mode忽略额外ACL读取权限 | 1 → 0 | `go test ./internal/review -run TestCycle16 -count=1` |
| 17 | 配置检查接受适配器会拒绝的URL | 1 → 0 | `go test ./internal/review -run TestCycle17 -count=1` |
| 18 | 配置导入接受悬空符号链接输出路径 | 1 → 0 | `python3 -m unittest scripts.test_cycles.Cycles.test_cycle18_dangling_output_link_is_rejected` |
| 19 | 导入目录被替换后凭据写入错误位置 | 1 → 0 | `python3 -m unittest scripts.test_cycles.Cycles.test_cycle19_replaced_output_directory_cannot_redirect_keys` |
| 20 | 代理端口无效仍可导入配置 | 1 → 0 | `python3 -m unittest scripts.test_cycles.Cycles.test_cycle20_proxy_port_rejected_before_writing` |
| 21 | 备份复验完成后未再次检查有效期 | 1 → 0 | `go test ./internal/review -run TestCycle21 -count=1` |
| 22 | 评测摘要写入失败仍返回成功 | 1 → 0 | `go test ./cmd/review-eval -run TestCycle22 -count=1` |
| 23 | 未知使用状态被输出成日期和有效来源 | 1 → 0 | `go test ./cmd/cleanup-report -run TestCycle23 -count=1` |
| 24 | 证据没有动作范围仍可用于永久清空审核 | 1 → 0 | `go test ./internal/review -run TestCycle24 -count=1` |
| 25 | 带目录的basename保护模式静默失效 | 1 → 0 | `go test ./internal/assess -run TestCycle25 -count=1` |
| 26 | CLI没有传入已知引用的入口 | 1 → 0 | `go test ./cmd/cleanup-report -run TestCycle26 -count=1` |
| 27 | 请求上限遗漏提示词和协议封装 | 1 → 0 | `go test ./internal/review -run TestCycle27 -count=1` |
| 28 | 只读盘点允许把报告写进盘点范围 | 1 → 0 | `go test ./cmd/cleanup-report -run TestCycle28 -count=1` |
| 29 | 写入失败留下看似完成的半份报告 | 1 → 0 | `go test ./cmd/cleanup-report -run TestCycle29 -count=1` |
| 30 | MiniMax用量在判定结果中丢失 | 1 → 0 | `go test ./internal/review -run TestCycle30 -count=1` |

每条命令对应的原始失败/通过输出保存在本次任务 `work/thirty-cycles/NN-red.log` 与 `NN-green.log`；附带JSON记录命令、退出码及耗时。可用交付的机器可读记录核对每一轮。

## 组合验证

- 原相关基线：review、assess、cleanup-report、review-eval、store通过。
- 修复后：上述模块加review-smoke组合检查通过。
- 导入脚本原测试与新增测试共9项通过。
- 真实Service与执行器的安全审核集成用例，以及隔离代表用例通过。
- 新开发二进制在独立临时目录中完成HTTPS假模型拒绝、允许隔离、恢复及字节一致检查。
- 本次差异格式检查通过。

全部模型测试使用本机假服务。没有使用真实JEV/MiniMax，原累计仍11/12次、剩余1次；70元预算未重置，没有新增模型费用。

## 本次实质变化

1. 范围与引用：根保护、无效表达式、目录引用传播、明确引用输入和非法路径拒绝。
2. 候选质量：不再遗漏敏感凭据与常见源码；补齐可复核文档格式，不以文档类型强制删除或永久豁免。
3. 审核约束：动作匹配、等待与读取前后有效期、双读预算、物理目录身份及私有文件ACL检查。
4. 模型调用：专属端点信任根隔离、完整请求大小限制、严格用量字段、两模型用量输出。
5. 数据与报告：未知使用信息不伪装为真实日期，报告不能写进被盘点范围，写入失败不发布半份结果，摘要写错返回失败。
6. 配置导入：拒绝悬空输出链接；通过已打开的目录描述符写私有文件，避免路径替换重定向；无效代理端口提前拒绝。

## 兼容性与使用变化

- 审核证据每条现在必须包含 `action: "quarantine"` 或 `action: "purge"`，并与本次动作匹配。原无action记录不会静默赋予清空意义；同一文件如需两种动作，应分别准备相应证据。模型审核仍不产生执行授权，原授权与保留期依然必须满足。
- 私有凭据、证据和CA文件如带额外ACL或无法证明权限状态，将被拒绝读取。
- `cleanup-report --protect-prefix .` 保护整个范围；`--protect-pattern`仍是basename模式，带目录的表达式须改用 `--protect-prefix`，错误模式明确拒绝。
- `cleanup-report --reference relative/path`可重复指定已知引用的文件或目录。未提供引用或未能观察到引用，不构成无引用的证明。
- 报告输出必须位于盘点范围之外，且不能覆盖已有文件。先写临时文件，完整写入并同步后才发布。
- 已验证的私有CA按Endpoint独立应用；若显式配置共用的 `trust_ca_file`，它仍表示调用者有意共用这一信任根。

## 保持的边界与未验证项

正式清理保持暂停；未替换已安装制品，未修改正式规则或授权，未移动或清空真实文件，未提交、推送或部署。既有6个隔离对象不因这些修复取得新授权。

这些测试证明相应程序路径的行为，并不证明真实模型判断精度或“零误删”。没有重新运行全量发布、跨平台构建或服务器验收。已有空间清单也未重扫，旧数字仍是旧盘点快照，不冒称修复后的新清单。

真正清理前仍需补具体候选的用途/可恢复性证据，并完成真实双模型验收。当前工作提升了约束、可观测性和工具正确性，没有把“不知道”变成“可以删”。
