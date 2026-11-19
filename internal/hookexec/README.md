# hookexec — 编排式钩子执行

在现有 Helm 钩子模型（`helm.sh/hook*` 注解、`pkg/release/v1/util` 解析与排序）之上，
提供可解释顺序、确定失败动作、幂等重跑的执行语义。**默认关闭**，不影响既有执行路径；
无新注解体系（沿用 `helm.sh/hook-*` 约定），无新依赖。

## 固定语义（不允许留白）

### 顺序

- 同一事件内的钩子按 **权重升序、名称字典序** 排序，稳定排序，每次运行顺序一致
  （复用 `releaseutil.SortHooksByWeight`，与既有执行路径同一实现）。
- 默认（`PlanOptions{}`）为扁平序列，顺序与未开启编排时**完全一致**。
- 开启 `GroupByResource` 后，钩子按执行组隔离：显式 `helm.sh/hook-group` 优先，
  否则取清单的 `<namespace>/<kind>`。组间按组名字典序串行，**权重只在组内生效**——
  不同命名空间、不同资源类型的钩子不会互相排序，避免依赖倒置。

### 并发

- 所有钩子**严格串行**执行。同权重钩子不并行。此语义固定，不开放配置。

### 超时

- 超时按**单个钩子**计：从该钩子开始执行起算，到点即通过 context 取消打断。
- 取值优先级：`helm.sh/hook-timeout`（Go duration，如 `"90s"`）> `Executor.DefaultTimeout`；
  两者都未设置则不超时。注解值非法时静默回退默认值。
- 超时失败记录为 `Timeout`，与业务失败 `Failed` 明确区分。

### 失败

- **中途失败（fail-fast）**：失败点之后的钩子一律不启动，状态中标记为 `Skipped`，
  随即进入回滚路径。
- **回滚路径**：执行回滚事件的钩子序列。重试有界（`RollbackMaxAttempts`，默认 3），
  已成功的回滚步骤跨重试不重复执行。次数耗尽即停止，保留全部记录并返回错误——
  不无限重试，不吞错继续。

### 幂等重跑

- 每个钩子有幂等键：`<namespace>/<kind>/<name>@<manifest-sha256前8字节>`。
  同名同内容（同版本）的重复定义按一次执行处理并给出 warning；同名不同内容视为不同钩子。
- 重跑时，状态为 `Succeeded` 的钩子跳过并列入跳过清单，不产生二次副作用。
- 进程被杀/重启后，状态中遗留 `Running` 的记录被判定为"已执行但结果未知"，
  标记 `Unknown`、列入报告并重新执行（副作用无法确认，只能重跑）。

### 状态记录

- 状态独立于集群资源，持久化在本地文件（`FileStore`，建议置于 `helmpath.DataPath()` 下），
  按 `<release>-<event>-rev<N>.json` 命名，临时文件 + rename 原子写入。
- 每个钩子执行前先落 `Running`、结束后再落结果，保证崩溃现场可恢复。
- 状态文件损坏时**降级为全部重跑**并产生 warning，绝不静默跳过。
- 钩子输出截断到 `MaxLogBytes`（默认 4096，按 rune 边界截断）并置 `OutputTruncated`，
  长日志不会撑爆状态记录。

## 新增注解

| 注解 | 含义 | 默认 |
| --- | --- | --- |
| `helm.sh/hook-timeout` | 单钩子超时，Go duration | 用 `Executor.DefaultTimeout` |
| `helm.sh/hook-group` | 显式执行组（需开启 `GroupByResource`） | `<namespace>/<kind>` |

## 验证

- 单测覆盖：顺序稳定性、默认顺序与既有路径一致、分组隔离、去重、超时解析、
  fail-fast、超时与业务失败区分、幂等重跑、中断恢复、状态损坏降级、回滚有界重试、
  日志截断。
- 验收演练（`drill_test.go`）：六钩子发布在第六个钩子失败，输出执行序列、
  回滚序列、重跑跳过清单；另覆盖回滚路径自身失败的有界停止。

  ```bash
  go test ./internal/hookexec/ -run TestDrill -v
  ```

## 接入方式

`Executor` 通过 `Runner` 接口执行单个钩子，与 Kubernetes 客户端解耦；
生产接入时在 `pkg/action` 中以适配器调用现有 kube client 即可，默认路径不变。
