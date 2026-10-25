# VM 基础代码评审结论

本轮交付：VM 生命周期定义、状态内存 fake、共用契约测试和开发约定，不包含真实 QEMU 后端。

## 评审来源

- codexx：gpt-6.1-sol / medium；完成独立只读评审及修正后复核。
- Claude CLI：完成独立只读评审，输出 modelUsage 为 deepseek-flash[1m]，因此不能将其标为 Anthropic Claude 模型评审。
- 尝试 Sonnet 别名仍使用了上述环境映射；完整 claude-sonnet-4-6 调用在 180 秒后超时，没有取得评审结果。未修改用户的持久模型或凭据配置。

原文见 [codexx](vm-foundation-codexx.md)、[Claude CLI](vm-foundation-claude-cli.md)、[最终复核](vm-foundation-followup.md)。原文中的行号对应评审时版本，修正后可能变化。

## 意见处理

| 意见 | 判断和处理 |
|---|---|
| 首次并发 Create/Start 缺少共用测试 | 已补，fake 另验证实际副作用计数为一次 |
| 只验证预先取消，未覆盖阻塞时取消 | 已补资源锁超时；共用场景中取消一个等待者后，其他 16 个等待者仍获得相同结果 |
| 清理失败可能丢失退出结果 | 已补 Destroy 失败后 Wait 仍返回原结果的断言 |
| Destroy 注释与 Created 可销毁行为不一致 | 修正接口、实现注释和 README，明确 Created/Stopped 均可销毁 |
| Stop 与 Wait 对未知 epoch 返回不同错误 | 不改为统一错误；明确 Stop 检查当前控制目标，返回 ErrStaleInstance；Wait 查找运行记录，返回 ErrNotFound，并补共用测试 |
| 非法 GracePeriod 未进入共用测试 | 已补零值、负值及运行状态不变的检查 |
| 停止中取消和旧 epoch Exit 覆盖不足 | 已补 fake 专有测试 |
| Starting/Stopping 可观察性不明确 | 明确允许观察到瞬时状态，但不保证每次过渡可见，不据此判断操作完成 |

有效修正集中在契约说明和测试，没有增加防御性兜底或第三方依赖。codexx 最终复核未发现新增实际问题。

## 验证

在 Go 1.27.3 下由主 Agent 执行：

- go test ./...：通过。
- go vet ./...：通过。
- CGO_ENABLED=1 go test -race ./...：通过。
- 方法注释、接口方法注释和每个结构体字段注释的检查：通过。

race 使用项目本地安装的 GCC 与头文件。Fake 不执行真实进程，不证明隔离安全；真实 QEMU 和多平台验收由后续实现承担。
