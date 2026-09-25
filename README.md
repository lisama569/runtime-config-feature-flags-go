# runtime-config-feature-flags-go

运行时配置与功能开关加载组件。用本地文件（`FileSource`）与进程内配置源
（`MemorySource`）模拟多来源配置，保证不同来源的覆盖顺序、动态更新与灰度判定
在各入口下得到一致且可解释的结论。

## 包结构

- `config`：多来源配置的分层加载、合并、校验与原子更新。
- `featureflag`：基于规则的功能开关灰度判定，结论稳定可复现、可解释。

## 配置来源优先级与合并语义

`config.NewStore(schema, sources...)` 按**参数顺序从低优先级到高优先级**排列来源。
合并时高优先级来源的同名键覆盖低优先级来源；每个键记录其最终生效来源，用于错误定位。

```go
store, err := config.NewStore(schema,
    &config.FileSource{Path: "base.json"},      // 低优先级：默认值
    &config.FileSource{Path: "override.json"},  // 中优先级：环境覆盖
    &config.MemorySource{SourceName: "remote", Values: vals}, // 高优先级：模拟远程下发
)
```

合并结果逐键按 `Schema` 校验：

| 拒绝原因 (`config.Reason`) | 含义 |
|---|---|
| `source_io` | 来源读取失败（文件不存在、JSON 非法、源不可用） |
| `type_mismatch` | 值类型与 schema 声明不符 |
| `missing_required` | 必填键在所有来源中均缺失 |
| `invalid_value` | 取值非法（越界、未在 schema 中声明的键等） |

**任何校验失败都不会静默回退默认值**，而是返回 `*config.RejectError`，
可用 `errors.As` 区分原因。声明为 `Sensitive: true` 的字段，其值不会出现在
错误信息与 `Snapshot.String` 输出中（以 `<redacted>` 代替）。

## 动态更新与回滚语义

- `store.Snapshot()` 返回当前生效的不可变快照，读取无锁、可并发。
- `store.Reload()` 重新加载全部来源：**先完整构建并校验新快照，全部成功才原子切换**；
  任一来源失败或校验失败则整体放弃，当前生效配置保持为上一份（快照指针不变）。
- 读取方永远不会观察到半更新状态；每次成功更新版本号单调递增。
- 并发读写与更新通过 `go test -race` 验证无数据竞争。

## 功能开关分流规则

```go
engine, err := featureflag.CompileWithVersion([]featureflag.Flag{{
    Name: "checkout_v2", Enabled: true,
    Rules: []featureflag.Rule{
        {Priority: 10, Percentage: 50, Salt: "a"},
        {Priority: 5,  Percentage: 80, Salt: "b"},
    },
}}, 1)
d, err := engine.Evaluate("checkout_v2", "user-42")
```

- **分流算法**：对 `flag + salt + target + version` 做 FNV-1a 64 位哈希，
  映射到 `[0,100)` 的桶；桶值小于规则 `Percentage` 即命中。
  同一目标在同一规则版本下结论**稳定可复现**；调整 `Salt` 或版本可重新分流。
- **优先级**：同一开关内规则按 `Priority` 从大到小依次判定，第一条命中的规则
  决定结论；优先级必须唯一，重复优先级在 `Compile` 时以
  `ReasonPriorityConflict` 拒绝。
- **失败即拒绝**：规则非法（百分比越界、开关重名等）在编译期报错；
  判定时不存在的开关返回 `ReasonUnknownFlag` 错误，**绝不默认放行**；
  未命中任何规则或开关未启用时结论为不放行。
- **可解释性**：`Decision` 携带生效版本、命中规则下标、桶值与人类可读的
  `Explain` 字段，单测日志会打印输入、生效版本与判定依据。

## 本地验证

```bash
go build ./...          # 编译
go vet ./...            # 静态检查
go test ./... -v        # 单元测试（日志含输入、生效版本与判定依据）
go test ./... -race     # 竞态检测（覆盖并发读写与更新）
```

测试覆盖：多来源覆盖优先级、错误原因分类、敏感字段脱敏、非法配置导致的回滚、
分流稳定性与比例统计、并发读写与并发判定（`-race` 下通过）。
