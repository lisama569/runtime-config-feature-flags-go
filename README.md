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

## 订阅变更通知

除了反复拉取 `Snapshot()`，还可以订阅每次**真正生效**后的更新：

```go
sub := store.Subscribe(16) // 参数为每订阅方通道缓冲；<=0 用默认值 16
go func() {
    for ev := range sub.Channel() {
        // ev.Version       新生效版本号
        // ev.Snapshot      新生效的完整不可变快照
        // ev.Changes       本次相对上一版本的逐键变动明细（按键名排序）
    }
}()
store.Unsubscribe(sub) // 随时退订，可重复调用
```

`KeyChange` 逐键给出：

| 字段 | 含义 |
|---|---|
| `Kind` | `added`（新键）/ `updated`（值变化）/ `removed`（键从所有来源消失） |
| `Source` | 该键**最终由哪个来源说了算**（删除键为其旧来源） |
| `Old` / `New` | 旧值与新值；新增键 `Old=nil`，删除键 `New=nil` |
| `Sensitive` | 敏感字段为 `true`，此时 `Old/New` 一律是 `"<redacted>"`，绝不露出真值 |

语义与边界：

- **只在更新成功生效后通知一次**；来源读取失败或校验失败、未生效的更新不产生任何通知，版本号也不前进。
- 订阅**不补发**注册之前的事件；需要对账时在订阅后主动读一次 `Snapshot()`。
- 订阅/退订可与并发更新安全交叉。**退订不关闭通道**（关闭会与并发扇出竞争
  send-on-closed），而是摘除注册表并标记非活跃；订阅方停止接收即可。
- **削峰策略（非阻塞扇出）**：每次通知对每个订阅方做一次非阻塞发送。
  某订阅方缓冲已满（处理太慢）时，**该事件对该订阅方直接丢弃**并累加
  `sub.Dropped()`，更新流程立即返回，不被任何慢订阅方拖住，也不影响其他订阅方。
  被丢弃的只是中间变动；订阅方随时可从最新事件携带的完整 `Snapshot` 自行对账。
  即“持续积压超过缓冲容量的那次更新会丢消息”，正常速度的订阅方不丢。

## 历史版本与按版本读取

```go
store, _ := config.NewStoreWithOptions(schema,
    []config.Source{base, override},
    config.WithHistoryRetain(20), // 保留最近 20 个已生效版本（含当前，默认 10，最小按 1）
)
snap, err := store.HistoryVersion(7) // 取回当时那份只读快照
```

- 保留窗口内版本号连续；超过上限后**最老的版本先淘汰**。
- 版本号错误用两种可区分的类型（`errors.As`）：
  - `*config.VersionNotFoundError`：该版本**从未存在**（大于当前版本）。
  - `*config.VersionEvictedError`：该版本曾经生效，但**已被淘汰**（早于窗口中最老版本）。
- 取回的历史快照不可变，后续更新不会改动它。

## 回滚语义

```go
if err := store.Rollback(3); err != nil { /* 版本不存在/已淘汰，错误分类同上 */ }
```

- 回滚使用的是**当时那份已确认生效的快照内容**，**不重新读取任何来源**：
  即使来源此刻已读不到或被改坏，回滚照样成功。
- 回滚后该内容成为当前生效配置；**版本号在当前基础上 +1，绝不倒退**
  （当前 v5 回滚到 v3 的内容，产生的是 v6）。
- 回滚与正常更新走同一条生效路径，因此**一样通知订阅方**，
  事件里的 `Changes` 描述从当前版本到回滚内容的差异；回滚产物同样进入历史窗口。

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

## 功能开关热更新与按版本重放

`featureflag.Manager` 在不可变 `Engine` 之上支持运行期整体替换规则集：

```go
m, err := featureflag.NewManager(flags)                  // 初始规则版本 1
m, err := featureflag.NewManagerAtVersion(flags, 1)      // 或显式指定起始版本（>0）

if _, err := m.Replace(newFlags); err != nil {           // 运行期整体换掉，版本 +1
    // 非法规则：沿用 Compile 的校验与错误分类，当前规则集保持不变
}
d, err := m.Evaluate("checkout_v2", "user-42")           // 用当前规则判定
old, err := m.EvaluateAt(1, "checkout_v2", "user-42")    // 按历史版本重放
```

- **原子热替换**：先完整编译校验新规则集，全部合法才用单个原子指针切换；
  并发判定要么看到换之前那一版、要么看到换之后那一版，**绝不会读到两版混合结果**。
  新版本 = 当前版本 +1；替换被拒时版本不前进、当前规则集不变。
- **同版稳定**：单次判定基于一个不可变 `Engine`，同一目标在同一版规则下结论
  稳定可复现。
- **按版本重放**：所有生效过的规则版本都被保留，`EvaluateAt` 用当时的引擎原样
  重算，结论与当时完全一致（版本参与分流哈希）。指定**不存在的版本**返回
  `ReasonUnknownVersion` 错误，绝不悄悄回退到当前版本或用默认值顶上。
- **错误语义不被搅乱**：热替换复用 `CompileWithVersion` 的全部校验，
  以前会被拒的非法规则（百分比越界、优先级冲突、开关重名等）现在同样被拒、
  原因同样可区分；不存在开关仍是 `ReasonUnknownFlag`，绝不默认放行。

## 兼容范围

- 原有调用方式全部保留可用：`config.NewStore` / `Snapshot()` / `Reload()`、
  `featureflag.Compile` / `CompileWithVersion` / `Engine.Evaluate` 签名与行为不变；
  不订阅、不配历史保留（默认 10）、不用 `Manager` 时，行为与旧版本一致。
- 新增能力均为可选入口：订阅（`Subscribe/Unsubscribe`）、历史与回滚
  （`HistoryVersion/Rollback`、`WithHistoryRetain`）、开关热更新与重放
  （`Manager`）。

## 本地验证

```bash
go build ./...              # 编译
go vet ./...                # 静态检查
go test ./... -v            # 单元测试（日志含输入、生效版本与判定依据）
go test ./... -race         # 竞态检测（并发读写/更新、订阅/退订、规则热替换）
go test ./... -race -count=3   # 多轮重复以稳定复现潜在竞态
```

测试覆盖：

- 配置：多来源覆盖优先级、错误原因分类、敏感字段脱敏、非法配置导致的回滚。
- 订阅：变动明细（新增/更新/删除、生效来源、旧值新值、敏感脱敏）、
  更新失败不通知、订阅/退订与更新并发交叉（`-race`）、
  慢订阅方丢弃计数且不拖住更新与其他订阅方。
- 历史与回滚：保留窗口与最老版本淘汰、按版本读取区分
  `VersionNotFoundError` / `VersionEvictedError`、回滚版本号单调前进并照常通知、
  **来源全部不可用时回滚仍成功**。
- 开关：分流稳定性与比例统计、规则热替换期间并发判定只见整版规则、
  按历史版本重放与当时结论一致、未知版本报 `ReasonUnknownVersion`、
  非法规则在热替换时同样被拒。
