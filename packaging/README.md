# packaging

这里**没有**预先渲染好的 unit 文件。渲染结果里嵌着二进制路径与状态目录，在一台机器上生成的模板装到另一台
机器上必然指向错误的位置。所以这里只放两个脚本，让模板在**目标机器上**生成。

| 文件 | 作用 |
|---|---|
| `install.sh` | 在目标机器上渲染 systemd / launchd 模板，装到用户级目录，可选立即启用 |
| `uninstall.sh` | 先停调度、再 `pause`、确认无 pending，最后移除 unit 文件 |

```sh
./packaging/install.sh --dry-run      # 只打印将要做什么
./packaging/install.sh                # 装好并启用
./packaging/install.sh --no-enable     # 只装文件，不启用
./packaging/uninstall.sh --dry-run
./packaging/uninstall.sh
```

## 为什么由脚本做，而不是程序自己装

程序本身只做一件事：`dustkeeper schedule render` 输出模板并退出。它**永不**执行 `systemctl`、
`launchctl` 或 `sudo`。改动系统状态的动作放在这两个脚本里，由操作者运行、在终端里看得见。

脚本遵守的几条：

- 拒绝以 root 运行。用 root 清理用户文件，会把文件变成用户无法恢复的所有物。
- `ExecStart` 用二进制的真实绝对路径（已解析符号链接）。换路径要重新生成。
- `install.sh` 里的定时时间默认 03:30，可用 `--hour` / `--minute` 改。
- `uninstall.sh` **不会**删除状态目录或任何隔离区，除非显式 `--purge-state`；而 `--purge-state` 也只删
  状态目录，从不碰 `.dustkeeper/objects`。
- `uninstall.sh` 发现有 pending 操作就停下，要求先 `recover reconcile`；`--force` 才会继续，并明确警告
  后果（可能留下无人解释的已移动对象）。

## 平台差异

- **systemd（用户级）**：装在 `~/.config/systemd/user/`。用户级 timer 在最后一段会话结束时停止；要让它
  在无人登录时也跑，得自己决定是否 `loginctl enable-linger`。脚本只提示，不代做。
- **launchd**：装在 `~/Library/LaunchAgents/`。通过 launchd 启动时没有终端，磁盘访问权限要授予**二进制本身**，
  不是授予终端。装完跑一次 `dustkeeper doctor` 确认。

完整操作流程见 [`../docs/operations.md`](../docs/operations.md) 第 7 节。
