# 10轮安全清理改进

状态：10轮本地改进与定向验证完成，未部署；正式清理暂停；真实模型累计11/12次，预算70元不重置。
目标：以证据筛选可清理候选并报告真实空间口径，不将“不知道”变成删除许可。

|轮次|问题与改进|本地证据|
|---|---|---|
|1|旧mtime不证明未使用；近期访问/使用/修改信号优先保留|TestRound1OldModificationDoesNotProveUnused：先缺实现失败，完成后通过|
|2|区分临时名称、明确临时来源、安装包和资料保护信号；分类不授权删除|TestRound2TemporaryNameIsNotDisposableEvidence：红→绿|
|3|打开中、已知引用、保护路径、非普通文件及多链接直接保留；缺引用记录仍是未知|TestRound3KnownUseAndUncertaintyNeverAuthorizeDelete：红→绿|
|4|分开逻辑大小、已分配块上限和隔离释放0；不承诺APFS共享块实际回收量|TestRound4SpaceIsAnUpperBoundAndIsolationFreesZero：红→绿|
|5|有界只读扫描，不跟随符号链接、不进入vault/代码依赖/应用包；限额或读错明确不完整|TestRound5BoundedReadOnlyScanDoesNotFollowLinksOrVault：缺实现→通过|
|6|增加有时限/输出上限的可见进程打开记录和Spotlight最后使用探针；缺失/失败保持未知|TestRound6UnavailableUsageAndProcessProbeRemainUnknown：缺实现→通过|
|7|新增只读cleanup-report：按占用上限排序、保留缺口、输出私有JSON、不创建删除计划|TestRound7ReadOnlyCLIProducesReviewListNotDeletePlan：缺实现→通过|
|8|模型输入标明本地已验证副本、用户声明、尚未知的使用/引用；不把声明冒充程序证据|TestRound8ModelFactsDistinguishVerifiedCopyFromUnknownUsage：缺字段→通过；未真实调用模型|
|9|反例发现照片曾被列为候选；补齐照片/视频/数据库/源码/配置保护，恶意文件名不能生成授权|TestRound9IrreplaceableAndAdversarialFilesStayProtected：照片反例红→绿；相关assess/CLI用例通过|
|10|真实Downloads只读盘点发现字体/电子书/日历误候选及保护目录遍历耗尽限额；修复后在排除范围内完成盘点|两个新增Round10反例红→绿；保留前两份不完整结果，最终范围内完整；零真实文件操作、零模型调用|

## 最终真实只读盘点

范围：Downloads，30天近期活动窗口，最多6000个目录项；排除pdf/、.dustkeeper、.git、node_modules、应用包；保护*-000.zip和bill_overview*。最终visited=5664、跳过7个目录，所声明排除范围内complete=true。

共3176条非目录记录：3046保留、130待复核（25安装包、8未知内容压缩包、97其它陈旧或不明文件）。待复核项分配空间上限7,359,369,216 B（约6.85 GiB），其中安装包6,568,947,712 B（约6.12 GiB）。这些不是确认可删数量或承诺回收量；APFS快照/克隆可能使实际回收更少。保守可保证回收下限0，实际释放0。

可见进程快照发现范围内17个打开路径，程序只使用其中实际扫描到的文件；不宣称已覆盖全部用户、程序或将来的引用。首次20个Spotlight最后使用探针均未得到日期，状态为未知，不表示未使用。最终扫描未重复这20次探针。

前两次盘点分别触及2000和6000目录项上限，结果保留为不完整。第三次是在修复“递归读取受保护pdf目录”的实际缺陷后完成。各次覆盖范围不同，不能用候选总数变化冒充准确率提升。

## 使用方式与含义

新增开发工具：

```sh
go run ./cmd/cleanup-report --root /Users/litany/Downloads \
  --max-entries 6000 --days 30 --open-files --spotlight-limit 20 \
  --protect-prefix pdf --protect-pattern '*-000.zip' \
  --protect-pattern 'bill_overview*' --output /absolute/new/report.json
```

工具只读元数据、按需读取可见打开记录和Spotlight索引；没有模型调用、数据库授权、隔离或删除能力。输出文件新建且0600，拒绝覆盖。输出中每个候选的can_delete恒为false，它不是审批计划。

- keep：近期活动、正在打开、已知引用、保护或资料类型等正向保留信号。
- needs_review：值得核实的空间机会，不是“已经没用”。
- unknown：缺少历史或引用覆盖证据；不能升级成“未使用/无引用”。
- declared_temporary_origin：只有明确传入temporary-prefix才标来源声明，仍不自动授权删除。

模型输入新增local_evidence，把本地核验的保留副本事实与operator_declared用途/理由、unknown使用史/引用覆盖分开。JEV仍需自身选择allow_review且confidence、P(allow_review)均>=0.9，MiniMax仍独立复核。没有因正例不通过而降低阈值。

## 验证与交付限制

每轮有定向失败/缺实现证据及修复后的通过记录，见上表。新只读扫描/分类/CLI测试通过；涉及模型输入变更的HTTP适配器假服务测试与Service→执行器组合测试通过。未执行全量回归、服务器或发布制品验收，未宣称模型识别准确率提升了多少。

这10轮是本地程序行为与候选证据的增量改进，不是10轮付费模型训练或评测。本轮模型请求0次；累计仍11/12，剩余1次，70元总预算未重置。

正式实例继续暂停，原安装不替换，现有6个隔离对象不动。代码不提交、不推送。新cleanup-report是开发侧只读入口，尚未纳入已发布制品。

## 如何真正让空间变大

优先核实25个安装包候选（约6.12 GiB分配空间上限），而不是先碰字体、课程、日历、源码或用户资料。逐项确认仍需旧版本与否、可靠再获取来源或已有独立副本、当前使用和已知引用，再按既有审核与授权流程处理。单纯同卷隔离不释放空间；专门在同卷复制一份备份再删原件，也不能宣称净腾出了同等空间。

当前尚未完成自动核验安装包官方下载来源/签名和真实恢复能力，因此本轮没有把这25项送去执行，也不能承诺现在已安全释放6.12 GiB。下一轮应围绕这些具体候选补可恢复性与用户用途证据，保留不确定项，不扩大到全盘猜测。

## 参考依据

- [Apple 最后使用时间元数据](https://developer.apple.com/documentation/coreservices/kmditemlastuseddate)：使用痕迹来源，不代表完整访问历史。
- [Linux mount/atime语义](https://www.man7.org/linux/man-pages/man8/mount.8.html)：访问时间可能采用relatime或noatime，旧时间不能证明没有访问。
- [Apple APFS说明](https://developer.apple.com/documentation/foundation/about-apple-file-system)：克隆与空间共享使逻辑大小、分配块和最终释放量不能简单等同。
