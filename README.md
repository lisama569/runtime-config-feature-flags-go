# runtime-config-feature-flags-go

运行时配置与功能开关加载组件。用本地文件与进程内配置源模拟多来源配置，
保证不同来源的覆盖顺序、动态更新与灰度判定在各入口下得到一致且可解释的结论。

## 包结构

- `config/`：多来源配置的分层加载、合并、校验与原子更新
- `featureflag/`：功能开关定义、灰度分流判定与可解释结论

## 配置来源与优先级

`config.Loader.Sources` 是一个有序列表，**越靠后的来源优先级越高**，
合并时后者覆盖前者的同名字段：

```go
loader := &config.Loader{
    Sources: []config.Source{
        config.FileSource{Path: "base.json"},        // 最低优先级：默认配置
        config.FileSource{Path: "override.json"},    // 环境覆盖
        config.MemorySource{Label: "dynamic", Values: m}, // 最高优先级：进程内动态配置
    },
    Schema: schema,
}
```

合并语义：按来源顺序逐层叠加（map 键覆盖），合并结果再整体交给 Schema 校验。
任一来源读取/解析失败，本次加载直接失败，不产生半成品配置。

## 校验与拒绝原因

校验失败绝不静默回退默认值，而是返回 `*config.RejectError`，
其 `Kind` 字段区分三类可编程判断的拒绝原因：

| Kind | 含义 |
|---|---|
| `KindTypeMismatch` | 字段类型与 Schema 声明不符 |
| `KindMissingRequired` | 必填字段缺失 |
| `KindInvalidValue` | 取值未通过自定义校验（如端口越界） |

声明 `Sensitive: true` 的字段（如密码）在错误信息与日志中一律显示为
`<redacted>`，真实值不会泄露。

## 动态更新、原子生效与回滚

`config.Store` 提供并发安全的读写：

- **读路径无锁**：`Current()` 通过 `atomic.Pointer` 返回不可变快照，
  读取方永远不会观察到半更新状态。
- **写路径互斥**：`Update()` 重新走完整的「加载 → 合并 → 校验」流程，
  全部通过后才原子替换当前快照并递增版本号。
- **失败回滚**：更新失败时整体放弃新配置，旧快照继续生效，版本号不变。

```go
store, _ := config.NewStore(loader) // 初始加载失败则直接报错
snap := store.Current()             // 任意时刻拿到一致的快照
if err := store.Update(); err != nil {
    // err 为 *RejectError 时可区分拒绝原因；旧配置仍在生效
}
```

## 功能开关与灰度分流

`featureflag.Registry` 在构建时整体校验，以下情况**明确报错而非默认放行**：

- 规则百分比越界（不在 `[0,100]`）
- 同一开关内规则优先级冲突（Priority 重复）
- `DependsOn` 引用了不存在的开关
- 开关重名

判定规则（`Registry.Evaluate(flag, target)`）：

1. 开关不存在 → 返回错误；
2. 开关全局关闭 → 不生效；
3. 依赖的开关未命中 → 不生效；
4. 规则按 Priority 升序评估，先命中者生效：
   - 目标在 `Allowlist` 中 → 直接命中；
   - 否则按 `FNV-1a(flag:version:target) % 100` 计算桶位（0-99），
     桶位 < `Percentage` 则命中。

**稳定性**：桶位由 `(开关名, 规则版本, 目标ID)` 的哈希决定，同一目标在同一
规则版本下的结论稳定可复现；版本号变化后按新哈希确定性重新分桶。

每次判定返回 `Decision`，包含 `FlagVersion`、`Bucket`、`Reason` 字段，
可完整解释单次判定的依据。

## 本地验证

```bash
go build ./...          # 编译
go test ./... -v        # 运行测试，日志中打印输入、生效版本与判定依据
go test ./... -race     # 竞态检测（并发读写与更新场景）
go vet ./...            # 静态检查
```

测试覆盖：

- `config`：多来源合并优先级、三类拒绝原因区分、敏感字段脱敏、
  非法配置更新回滚（版本号不变）、并发读写更新（`-race` 下无数据竞争）；
- `featureflag`：分流稳定性（同目标同版本重复判定结论一致）、
  未知开关/非法规则/优先级冲突/悬空依赖的明确报错、依赖门控、
  并发判定（`-race`）。
