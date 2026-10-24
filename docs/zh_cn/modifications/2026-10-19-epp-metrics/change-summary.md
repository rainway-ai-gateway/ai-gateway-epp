# EPP 指标导出修复与池/端点指标补齐（`llm_d_epp_*` / `ai_epp_per_endpoint_queue_size`）变更摘要

## 1. 背景

EPP 的 `http://<host>:9004/metrics` 上只有 ai-gateway 自有指标（`ai_epp_*`）与插件自注册指标，**看不到任何 `llm_d_epp_*`**。根因不是采集器没写，而是**组装入口从未把 llm-d 的采集器注册进 `:9004` 导出的 registry**：

1. `:9004` 导出的是 controller-runtime 的 `ctrlmetrics.Registry`；而 `llm_d_epp_*` 采集器**只在**上游 `llm-d-router/pkg/epp/metrics.Register()` 里 `MustRegister` 到该 registry（`sync.Once` 保护）。
2. 上游 EPP 在其 runner 里调用 `metrics.Register(...)`；`ai-gateway-epp` 全仓**没有**任何一处 import `pkg/epp/metrics` 或调用 `Register` → `RecordRequest*` / `RecordFlowControl*` / `RecordDataLayerPollError` 等仍在执行，但写入的是**未注册**的采集器，故 `/metrics` 不导出。

在此之上还有两条"**生产者缺失**"（注册后仍缺失，属于另一类问题）：

3. **池聚合 7 指标**（`llm_d_epp_ready_endpoints` / `average_kv_cache_utilization` / `average_queue_size` / `average_running_requests` / `std_dev_{kv_cache_utilization,queue_size,running_requests}`）的唯一生产者是上游 `pkg/epp/datalayer/logger.StartMetricsLogger`（由上游 runner 启动）；ai-gateway 自有的组装根从未启动它 → 函数从未被调用 → 空 `GaugeVec` 不导出。
4. **端点级排队数** `llm_d_epp_per_endpoint_queue_size` 上游是"**每 datastore 一个**"的定制 collector；ai-gateway 是**多 cell（多 datastore）**模型，且 cell 运行时增删，直接照搬会踩重复 Desc / `sync.Once` / 生命周期配对等问题。

对照源码注册集与实际导出集，注册修复后仍有约 25 个家族缺失，可归为四类：①**池聚合 7 个是真缺口**（生产者循环未启动，本次修复）；②事件驱动（在错误/流式等条件发生前不产出样本）；③配置/特性未启用（如全局 maxBytes、composite 探测器、驱逐未开启）；④未实现（端点级明细，本次以 `ai_epp_` 前缀单独补）。

## 2. 目标

- 让 `:9004/metrics` 完整导出 `llm_d_epp_*`：**最小、可回滚、无行为副作用**（仅新增指标导出）。
- 补齐池聚合 7 指标，**按集群区分**（多 cell 各自成组）。
- 以**多 cell 友好**的方式暴露端点级排队数：单一注册、无 cell 生命周期耦合、无 panic/泄漏风险。

## 3. 范围

| 范围 | 说明 |
|------|------|
| 涉及仓库 | `ai-gateway-epp`（本仓库）；上游 `llm-d-router` 仅**复用**（`pkg/epp/metrics`、`pkg/epp/datalayer/logger`、`pkg/common/observability/metrics`），**零改动** |
| 主要模块 | `cmd/epp/`（`main.go` / `config.go`）、`pkg/cell/`（`metrics.go` / `manager.go`）、`go.mod`、单元测试 |
| 接口消费 | 无对外契约变更；仅新增指标导出与三个进程 flag |
| 发版依赖 | 无跨仓同步要求：只消费上游既有 API（`metrics.Register` / `RecordInferenceExtensionInfo` / `StartMetricsLogger` 在 v0.10/v0.11 均存在） |

## 4. 关键决策

| 决策 | 说明 |
|------|------|
| 组装入口一次性注册 | 在 `run()` 中 `registerPlugins(hub)` 之后、启动 server 之前调用 `eppmetrics.Register()`：早于 `/metrics` server，首个 scrape 即可见；`Register` 内部 `sync.Once` 保证幂等，与插件自注册（走 `handle.Metrics()`）互不冲突；进程级一次注册，**与 cell 数无关**。 |
| fairness_id 上限参数化 | 新增 `-fairness-id-metric-label-limit`（默认 `metricsutil.DefaultFairnessIDLabelLimit`=1000），在 `Register()` **之前**调用 `metricsutil.SetFairnessIDLabelLimit`（该函数是**整体替换**，必须启动时一次）。默认值与上游一致，不配置即保持现状。 |
| `RecordInferenceExtensionInfo(commit, version)` 顺序 | 指标标签为 `{commit, build_ref}`，签名为 `(commitSha, buildRef)`；`main.version="v0.0.2"`、`main.commit=<sha>`。必须传 `(commit, version)` 才能得到 `commit=<sha>, build_ref="v0.0.2"`；写成 `(version, commit)` 会颠倒语义。 |
| 端点级明细用**整进程单 collector** | 只注册 1 个 collector，`Collect` 时 `manager.List()` 快照并遍历所有 cell 产出 `ai_epp_per_endpoint_queue_size{cluster,inference_pool,model_server_endpoint}`。**不采用**每 cell 注册：① 多个 cell 用同一 `Desc` 会因 `Desc.id = fqName + const 标签值` 相同而判重 → `MustRegister` **panic**（把 `cluster` 作*可变*标签无法规避）；② `metrics.Register` 受 `sync.Once` 约束，后续 per-cell 调用静默无效；③ 与 `Ensure`/`Drop`/drain 异步路径的注册/注销配对极易泄漏。单 collector 把 cell 生命周期从注册表中彻底解耦。 |
| 池聚合用**每 cell 一个上游刷盘循环** | 在 `defaultNewCell` 里 `datalayerlogger.StartMetricsLogger(cellCtx, ds, ...)`。上游语义即"每 datastore 一个循环"，恰好对应 ai-gateway"每 cell 一个 datastore"；循环只看 `cellCtx`，`Cell.Close()`→`cancel()` 后退出，含 `LoadOrStore` 竞争败者路径，**无泄漏**。`pool.Name = string(cell.Key)`，天然按集群区分。 |
| 命名采用清晰名（B 方案） | 新增 `-pool-metrics-flush-interval`(5s) 与 `-pool-metrics-staleness-window`(2s)，内部映射到上游 `StartMetricsLogger` 入参（上游 `RefreshPrometheusMetricsInterval` / `MetricsStalenessThreshold`）。**刻意避开**两个撞名：与既有 `-refresh-metrics-interval`（50ms，数据源抓取）区分；与饱和度探测器 JSON 参数 `metricsStalenessThreshold`（默认 200ms，另一概念）区分。 |
| `NewManager` 兜底默认值 | `StartMetricsLogger` 内部 `time.NewTicker(interval)` 对 `<=0` 会 **panic**；既有测试经 `NewManager` 构造 `Options` 且未设置这两个字段、又未覆盖 `NewCell`，会走 `defaultNewCell`。故在 `NewManager` 里对 `<=0` 取 5s/2s（沿用同函数 `DrainTimeout`/`DrainWorkers` 的既有风格），`Config.Validate` 另作正数校验（快速失败）。 |

## 5. 非目标

- 不改动调度 / 准入 / 流控逻辑；仅新增指标导出。
- 不解决后端 `/metrics` 抓取失败（另一项独立问题）：池聚合的 `average_*` / `std_dev_*` 仍取决于"端点指标是否新鲜"，抓取失败时这些聚合不会产出、`ready_endpoints` 可能为 0。
- 不引入 Prometheus 之外的暴露方式；不回填历史数据。
- 不实现"指标随 cell 消失"的 per-cell 注册/注销语义；不导出带 per-cell 维度的池聚合（沿用 `name=<cluster>`）。
- 不复用上游 `llm_d_epp_per_endpoint_queue_size` 的名字/Desc（改用 `ai_epp_` 前缀，规避 `dimHashesByName` 终身一致性约束与语义混淆）。
- 不修改上游 `logger` / `metrics` 包。

## 6. 关联代码锚点

- 上游指标注册/记录：`llm-d-router/pkg/epp/metrics`（`Register`、各 `Record*`、`llm_d_router_metrics.go`）
- 上游池聚合刷盘：`llm-d-router/pkg/epp/datalayer/logger`（`StartMetricsLogger` / `refreshPrometheusMetrics`）
- 上游 fairness 上限与枚举：`llm-d-router/pkg/common/observability/metrics`（`SetFairnessIDLabelLimit`、`DefaultFairnessIDLabelLimit`）
- 上游饱和度探测器陈旧参数：`llm-d-router/pkg/epp/framework/plugins/flowcontrol/saturationdetector/utilization`（`metricsStalenessThreshold`）
- 本仓库改动：`cmd/epp/{main.go,config.go}`、`pkg/cell/{metrics.go,manager.go}`