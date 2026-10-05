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

## 变更订阅与变动明细

```go
sub, unsubscribe := store.Subscribe(16) // 缓冲容量 16
defer unsubscribe()
for ev := range sub.C {
    // ev.Snapshot：本次生效后的新快照
    // ev.Changes：相对上一份生效配置的变动明细
    // ev.Rollback：是否由回滚触发
}
```

- **只在生效后通知**：`Reload`/`RollbackTo` 真正切换了当前配置才发布事件；
  校验失败、来源读取失败等未生效的更新**不产生任何事件**。
- **变动明细（`KeyChange`）**：每个发生变化的键记录最终生效来源（`Source`）、
  旧值（`Old`）与新值（`New`）；新增键 `Old` 为 nil、被移除键 `New` 为 nil。
  敏感字段只标记 `Redacted: true`，`Old`/`New` 一律为 nil，**不泄露真值**。
- **削峰与丢弃策略**：发布对每个订阅方都是非阻塞发送。订阅方消费过慢、
  缓冲（`Subscribe(buffer)` 的容量）已满时，**该次事件对此订阅方直接丢弃**，
  更新流程与其他订阅方不受影响；丢弃数量可通过 `sub.Dropped()` 观察。
  需要不丢消息的订阅方应给出足够大的缓冲并及时消费。
- **并发安全**：订阅、退订与更新可任意并发交叉；退订函数幂等，
  退订后事件通道被关闭。

## 历史版本与回滚

- Store 自动保留最近若干个已生效版本（不含当前版本），默认
  `config.DefaultHistoryKeep = 16`，可用 `store.SetHistoryLimit(n)` 调整；
  超过上限后**最老的版本被淘汰**，调小上限会立即淘汰多余版本。
- `store.SnapshotAt(version)` 按版本号取回当时生效的快照（只读）。
  失败时返回 `*config.VersionError`，可通过 `Evicted` 字段区分两类原因：
  `Evicted=false` 表示该版本从未存在（或尚未达到），
  `Evicted=true` 表示曾生效但已被淘汰。
- `store.RollbackTo(version)` 回滚到某个历史版本：该版本的内容成为当前
  生效配置，**版本号继续单调递增**（当前版本号 +1，不倒退），并像正常更新
  一样向订阅方发布事件（`Event.Rollback=true`，变动明细照常计算）。
- 回滚**只读取内存中的历史快照，不访问任何配置来源**：即使来源此刻
  不可用或内容已被改坏，回滚照样成功——它恢复的是"当时已校验通过"的那份配置。

## 功能开关热更新与按版本重放

```go
m, err := featureflag.NewManager(flags, 8) // 初始版本 1，保留 8 个历史版本
d, err := m.Evaluate("checkout_v2", "user-42")   // 按当前版本判定
err = m.Update(newFlags)                          // 整体热替换，版本 +1
d, err = m.EvaluateAt(1, "checkout_v2", "user-42") // 按历史版本重放
```

- **原子替换**：`Update` 先完整校验新规则集（复用 `Compile` 的全部校验与
  错误分类），成功才原子切换；并发判定要么看到旧版、要么看到新版，
  **绝不会看到两版混杂的结果**。校验失败时当前规则集与版本号都不变，
  不会用默认值顶替。
- **按版本重放**：`EvaluateAt(version, flag, target)` 用指定历史版本的规则
  重算判定，结果与当时实时判定完全一致；版本不存在或已被淘汰时返回
  `ReasonUnknownVersion` 错误（可与 `ReasonUnknownFlag` 等既有类别区分）。
- 历史版本保留数量由 `NewManager(flags, keep)` 的 `keep` 指定，
  超出后最老版本被淘汰。

## 兼容范围

- 原有调用方式完全可用：`config.NewStore` / `Snapshot` / `Reload`、
  `featureflag.Compile` / `CompileWithVersion` / `Engine.Evaluate` 的签名与
  语义均未改变；校验规则与错误分类（`config.Reason`、`featureflag.Reason`）
  保持不变，以前会被拒的配置/规则现在同样被拒。
- 新增能力均为增量 API：`Store.Subscribe` / `SnapshotAt` / `RollbackTo` /
  `SetHistoryLimit`，以及 `featureflag.Manager`。

## 本地验证

```bash
go build ./...          # 编译
go vet ./...            # 静态检查
go test ./... -v        # 单元测试（日志含输入、生效版本与判定依据）
go test ./... -race     # 竞态检测（覆盖并发读写、订阅/退订与热替换）
```

测试覆盖：多来源覆盖优先级、错误原因分类、敏感字段脱敏、非法配置导致的回滚、
变更通知与变动明细（含敏感字段脱敏）、慢订阅方丢弃策略、并发订阅/退订与更新交叉、
历史版本保留与淘汰、按版本读取与回滚（含来源不可用时回滚）、开关规则热替换期间的
并发判定、按版本重放一致性，以及分流稳定性与比例统计（`-race` 下通过）。

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
