# EPP 指标导出修复与池/端点指标补齐 设计变更说明

变更记录见 `change-summary.md`。涉及模块：`cmd/epp/`（`main.go` / `config.go`）、`pkg/cell/`（`metrics.go` / `manager.go`）、`go.mod` 与单元测试。上游 `llm-d-router` 零改动。

本次改动分三组，均为"新增指标导出 + 相关配置"，不触碰调度 / 准入 / 流控：

1. 在组装入口注册 llm-d EPP 采集器（`llm_d_epp_*`）。
2. 新增端点级排队指标 `ai_epp_per_endpoint_queue_size`（多 cell）。
3. 接通池聚合刷盘循环（`llm_d_epp_ready_endpoints` / `average_*` / `std_dev_*` 共 7 个）。

---

## 1. 指标注册（`cmd/epp/main.go`）

**新增 import：**

```go
metricsutil "github.com/llm-d/llm-d-router/pkg/common/observability/metrics"
eppmetrics  "github.com/llm-d/llm-d-router/pkg/epp/metrics"
```

**在 `run()` 中插入**（`registerPlugins(hub)` 之后、`cell.NewManager` 之前）：

```go
// Register llm-d EPP metric collectors into the controller-runtime registry
// that the /metrics server exposes. Without this, none of the llm_d_epp_*
// metrics are exported even though Record* functions are called at runtime.
metricsutil.SetFairnessIDLabelLimit(cfg.FairnessIDMetricLabelLimit)
eppmetrics.Register()
eppmetrics.RecordInferenceExtensionInfo(commit, version)
```

要点：

- 位置**早于** `/metrics` server 启动，首个 scrape 即可见；`Register` 内部 `sync.Once` 幂等，与插件自注册（`handle.Metrics()` = `ctrlmetrics.Registry`）互不冲突；进程级一次注册，**与 cell 数量无关**。
- `SetFairnessIDLabelLimit` 必须**在 `Register` 之前**、且**启动时一次**：它是**整体替换** `atomic.Pointer`（非扩容），运行时反复调用会清空已接纳集合。
- **`RecordInferenceExtensionInfo(commit, version)`**：指标 `llm_d_epp_info` 的标签是 `{commit, build_ref}`，函数签名是 `(commitSha, buildRef)`。`main.version="v0.0.2"`、`main.commit=<git sha>`，故传 `(commit, version)` 才得到 `commit=<sha>, build_ref="v0.0.2"`；写成 `(version, commit)` 会颠倒语义。

## 2. 进程配置（`cmd/epp/config.go`）

**`Config` 新增字段：**

```go
// FairnessIDMetricLabelLimit caps the number of distinct fairness_id label
// values recorded on llm_d_epp_* metrics; values beyond the cap collapse to
// the overflow value. 0 folds every value onto the overflow series.
// Negative is rejected by Validate.
FairnessIDMetricLabelLimit int

// PoolMetricsFlushInterval is how often the inference-pool aggregate
// metrics (llm_d_epp_ready_endpoints, average_*, std_dev_*) are recomputed
// and exported. Must be positive.
PoolMetricsFlushInterval time.Duration
// PoolMetricsStalenessWindow is how old an endpoint's metrics may be to
// still count toward the pool aggregates. Must be positive.
PoolMetricsStalenessWindow time.Duration
```

**`parseConfig()` 新增 flag：**

```go
flag.IntVar(&cfg.FairnessIDMetricLabelLimit, "fairness-id-metric-label-limit", metricsutil.DefaultFairnessIDLabelLimit,
    "max distinct fairness_id label values on metrics; excess collapses to overflow")
flag.DurationVar(&cfg.PoolMetricsFlushInterval, "pool-metrics-flush-interval", 5*time.Second,
    "how often inference-pool aggregate metrics are recomputed and exported")
flag.DurationVar(&cfg.PoolMetricsStalenessWindow, "pool-metrics-staleness-window", 2*time.Second,
    "how old an endpoint's metrics may be to still count toward pool aggregates")
```

默认值对齐上游（1000 / 5s / 2s），不配置即保持现状。

**`Validate()` 新增校验：**

```go
if c.FairnessIDMetricLabelLimit < 0 {
    return fmt.Errorf("fairness-id-metric-label-limit must be non-negative, got %d", c.FairnessIDMetricLabelLimit)
}
if c.PoolMetricsFlushInterval <= 0 {
    return fmt.Errorf("pool-metrics-flush-interval must be positive, got %v", c.PoolMetricsFlushInterval)
}
if c.PoolMetricsStalenessWindow <= 0 {
    return fmt.Errorf("pool-metrics-staleness-window must be positive, got %v", c.PoolMetricsStalenessWindow)
}
```

> 命名说明：`-pool-metrics-flush-interval` / `-pool-metrics-staleness-window` 刻意与既有 `-refresh-metrics-interval`（50ms，**数据源抓取**间隔）和饱和度探测器 JSON 参数 `metricsStalenessThreshold`（默认 200ms，判端点饱和用）区分。三者名字相近但含义完全不同：
> - **采集**：`-refresh-metrics-interval`，多久去后端 `/metrics` 抓一次数；
> - **判定**：`-pool-metrics-staleness-window`，端点读数多旧仍算有效（超过则不计入池聚合）；
> - **上报**：`-pool-metrics-flush-interval`，池聚合值多久重算并导出一次。

## 3. 端点级排队指标（`pkg/cell/metrics.go`）

新增"整进程单 collector"（`import` 增加 `.../pkg/epp/datastore`）：

```go
var perEndpointQueueSizeDesc = prometheus.NewDesc(
	"ai_epp_per_endpoint_queue_size",
	"Current number of requests waiting in the model-server queue, per endpoint, per cluster.",
	[]string{"cluster", "inference_pool", "model_server_endpoint"},
	nil,
)

// perEndpointQueueSizeCollector is a process-wide collector that snapshots every
// live cell at scrape time. Registering it once avoids the duplicate-Desc panic
// and the sync.Once pitfalls of per-cell registration.
type perEndpointQueueSizeCollector struct {
	cells func() []*Cell
}

var _ prometheus.Collector = (*perEndpointQueueSizeCollector)(nil)

func (c *perEndpointQueueSizeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- perEndpointQueueSizeDesc
}

func (c *perEndpointQueueSizeCollector) Collect(ch chan<- prometheus.Metric) {
	for _, cell := range c.cells() {
		pool, err := cell.ds.PoolGet()
		if err != nil {
			continue // pool not synced yet; emit nothing
		}
		for _, ep := range cell.ds.PodList(datastore.AllPodsPredicate) {
			ch <- prometheus.MustNewConstMetric(
				perEndpointQueueSizeDesc,
				prometheus.GaugeValue,
				float64(ep.GetMetrics().WaitingQueueSize),
				string(cell.Key),
				pool.Name,
				ep.GetMetadata().ID.Name,
			)
		}
	}
}

// NewPerEndpointQueueSizeCollector builds the process-wide per-endpoint queue
// collector. cells is called on every scrape and must return a snapshot.
func NewPerEndpointQueueSizeCollector(cells func() []*Cell) prometheus.Collector {
	return &perEndpointQueueSizeCollector{cells: cells}
}
```

设计依据（为何"整进程一个"而非"每 cell 一个"）：

1. `prometheus.Desc.id` 只由 **fqName + const 标签值** 决定；多个 cell 用同一 `Desc` → `Desc.id` 相同 → 第 2 个 `MustRegister` **panic**。把 `cluster` 作*可变*标签也不能规避（可变标签名不进 `id`）。
2. `metrics.Register` 内部 `sync.Once`，只有第一次调用的 collector 会注册，per-cell 追加**静默无效**。
3. `Ensure`/`Drop`/drain 三条异步路径下，per-cell 注册/注销配对极易泄漏。

`Collect` 无自身可变状态；`manager.List()` 为 `sync.Map.Range` 快照；`PoolGet`/`PodList` 内部加锁，抓取可并发调用。标签语义：`cluster = cell.Key`、`inference_pool = pool.Name`（当前 `= string(key)`，与 `cluster` 相同，保留以便与 `llm_d_epp_flow_control_*{inference_pool=...}` 关联）、`model_server_endpoint = ep.GetMetadata().ID.Name`。

## 4. 池聚合刷盘循环（`pkg/cell/manager.go`）

**`Options` 新增字段：**

```go
// PoolMetricsFlushInterval is how often the per-cell inference-pool
// aggregate metrics are recomputed and exported.
PoolMetricsFlushInterval time.Duration
// PoolMetricsStalenessWindow is how old an endpoint's metrics may be to
// still count toward those aggregates.
PoolMetricsStalenessWindow time.Duration
```

**`NewManager` 兜底默认值**（防 `time.NewTicker(<=0)` panic，沿用 `DrainTimeout`/`DrainWorkers` 风格）：

```go
if opts.PoolMetricsFlushInterval <= 0 {
    opts.PoolMetricsFlushInterval = 5 * time.Second
}
if opts.PoolMetricsStalenessWindow <= 0 {
    opts.PoolMetricsStalenessWindow = 2 * time.Second
}
```

**`defaultNewCell` 启动刷盘循环**（`import` 增加 `datalayerlogger ".../pkg/epp/datalayer/logger"`）：

```go
rt := datalayer.NewRuntime(m.opts.RefreshMetricsInterval)
pool := datalayer.NewEndpointPool(m.opts.PoolNamespace, string(key))
ds := datastore.NewDatastore(cellCtx, rt).WithEndpointPool(pool)

// Pool aggregate metrics (ready_endpoints, average_*, std_dev_*), flushed
// per cell and torn down with cellCtx.
datalayerlogger.StartMetricsLogger(cellCtx, ds, m.opts.PoolMetricsFlushInterval, m.opts.PoolMetricsStalenessWindow)
```

要点：

- 上游语义"每 datastore 一个循环" ↔ ai-gateway"每 cell 一个 datastore"，一一对应。
- 循环只依赖 `cellCtx`：`Cell.Close()`（`Ensure` 竞争败者 / `Drop` / drain 最终路径）取消 → goroutine 退出，**无泄漏**；配置热重载只换 Engine、不重建 cell，不会重复启动。
- `ds`（`*datastore`）满足上游 `datalayer.PoolInfo`（`PoolGet` + `PodList`）。
- 产出：`llm_d_epp_ready_endpoints{name=<cluster>}` **总是**（`PoolGet` 成功即写，值可为 0）；`average_*` / `std_dev_*` **仅当有新鲜端点**（`UpdateTime` 在陈旧窗口内、`podCount>0`）。

## 5. 组装接线（`cmd/epp/main.go`）

```go
manager := cell.NewManager(ctx, cell.Options{
    // ... 既有字段 ...
    PoolMetricsFlushInterval:   cfg.PoolMetricsFlushInterval,
    PoolMetricsStalenessWindow: cfg.PoolMetricsStalenessWindow,
})

// Process-wide per-endpoint queue metric, snapshotted across all live cells.
ctrlmetrics.Registry.MustRegister(
    cell.NewPerEndpointQueueSizeCollector(func() []*cell.Cell { return manager.List() }),
)
```

端点级 collector **直接**注册到 `ctrlmetrics.Registry`，**不**经 `eppmetrics.Register`（后者受 `sync.Once` 约束，只有第一次调用入参生效，且其注册时机早于 manager 创建）。

## 6. 依赖（`go.mod`）

单元测试引入 `prometheus/client_golang/prometheus/testutil`，其传递依赖需要在 `go.mod` 声明：

```diff
 require (
   ...
+  github.com/kylelemons/godebug v1.1.0 // indirect
   ...
 )
```

`replace github.com/llm-d/llm-d-router` 未改动。

## 7. 测试用例

**单元测试 `pkg/cell/metrics_test.go`（新增）**

- 用内嵌 `datastore.Datastore` 接口的 stub（仅覆写 `PoolGet` / `PodList`）构造假 cell，无需实现整个大接口。
- 用例 1（正常聚合）：两个 cell（`cluster_epp_sim` 2 端点、`cluster_epp_real` 1 端点）→ 以 `testutil.CollectAndCompare` 断言恰好产出 3 条 `ai_epp_per_endpoint_queue_size`，标签 `{cluster,inference_pool,model_server_endpoint}` 与值（`WaitingQueueSize`）逐一正确。
- 用例 2（池未同步）：一个 `PoolGet` 返回错误的 cell → 断言其**不产出**任何样本（走 `continue` 分支）。

**配置校验 `cmd/epp/config_test.go`（更新 `TestConfigValidate`）**

- `Config{EppGroup:"g1"}`（两个池指标间隔为 0）→ 期望**报错**（`pool-metrics-flush-interval must be positive`）。
- `Config{EppGroup:"g1", PoolMetricsFlushInterval: 5s, PoolMetricsStalenessWindow: 2s}` → 期望 `nil`。
- 仅缺其中一个正数间隔 → 期望报错。

命令：`go build ./...`、`go vet ./cmd/... ./pkg/cell/...`、`go test ./pkg/cell/ ./cmd/...`。

## 8. 风险与注意事项

| 项 | 说明 |
|----|------|
| `ready_endpoints=0` 的语义 | 表示"**指标足够新鲜**的端点数"，**不是**调度候选门控；它不参与调度/准入，也不被任何内部代码读取（指标是只写导出）。 |
| `average_*` / `std_dev_*` 可能不出现 | 仅当 cell 有新鲜端点才产出；根因通常在后端 `/metrics` 抓取失败，需另行修复。 |
| 命名与上游不一致 | flag 名与上游不同（清晰优先），但默认值一致且内部映射到上游入参；help 文本说明等价关系。 |
| 陈旧窗口不影响调度 | `--pool-metrics-staleness-window` 只影响池聚合指标；调大只会让指标把陈旧端点也计入（失真），不改变候选/门控。 |
| 回归面 | 仅新增导出与 flag；调度/流控路径零改动；既有 `ai_epp_*` 与插件指标不受影响。 |

## 9. 涉及文件清单

| 文件 | 改动 |
|------|------|
| `cmd/epp/main.go` | 新增 import；调用 `SetFairnessIDLabelLimit` / `Register` / `RecordInferenceExtensionInfo`；传入两个池指标间隔；注册端点级 collector |
| `cmd/epp/config.go` | `Config` 三个新字段；三个 flag；`Validate` 校验 |
| `cmd/epp/config_test.go` | `TestConfigValidate` 适配"必须为正" |
| `pkg/cell/metrics.go` | 新增 `perEndpointQueueSizeDesc` + `perEndpointQueueSizeCollector` + `NewPerEndpointQueueSizeCollector` |
| `pkg/cell/metrics_test.go` | 新增（`testutil.CollectAndCompare`） |
| `pkg/cell/manager.go` | `Options` 两个新字段；`NewManager` 默认值；`defaultNewCell` 启动刷盘循环 + 新增 import |
| `go.mod` | 新增测试用间接依赖 `github.com/kylelemons/godebug` |

## 10. 非目标

见 `change-summary.md` §5（不改调度/流控、不解决后端端点指标抓取失败、不实现 per-cell 注册语义、不引入其它暴露方式、不复用上游端点指标名）。