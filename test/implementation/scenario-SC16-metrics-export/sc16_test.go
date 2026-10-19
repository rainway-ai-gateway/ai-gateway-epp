// Copyright(c) 2026 The Rainway AI Gateway (壬远AI网关) Authors.
//
//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//http://www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.

// Package sc16: SC16 指标导出（llm_d_epp_* / 端点级 / 池聚合）。
// 测试设计文档：test/测试设计文档/scenario-SC16-指标导出/
//
// 覆盖 2026-10-19 指标改动：组装入口注册 llm-d EPP 采集器（llm_d_epp_*）；
// 新增端点级排队指标 ai_epp_per_endpoint_queue_size；接通池聚合刷盘循环
// （llm_d_epp_ready_endpoints / average_* / std_dev_*）。
//
// TC-01~TC-04 采用本地配置文件模式（不启动 inference-sim，cluster_table
// 端点为保留回环端口），只验证指标导出与配置项，不访问推理后端；
// TC-05 需要 inference-sim（缺失时 t.Skip），验证"新鲜端点"驱动的池聚合产出。
package sc16

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rainway-ai-gateway/ai-gateway-epp/test/common"
)

// sample parses the value of an exact metric sample line (metric + full label
// set, without the trailing value). It reports ok=false when the sample is
// absent, so a legitimate value of 0 is distinguished from "no sample".
func sample(text, prefix string) (v float64, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix+" ") {
			fmt.Sscanf(line[len(prefix)+1:], "%g", &v)
			return v, true
		}
	}
	return 0, false
}

// hasFamily reports whether any sample of the given metric family is present.
func hasFamily(text, name string) bool {
	return strings.Contains(text, "\n"+name+"{") || strings.HasPrefix(text, name+"{")
}

// TestTC01_CollectorsRegistered: llm-d EPP collectors are registered into the
// registry served at /metrics. Without the startup registration none of the
// llm_d_epp_* families are exported even though Record* runs at runtime.
func TestTC01_CollectorsRegistered(t *testing.T) {
	e := common.NewLocalEnv(t, "epp-sc16-tc01", map[string][]string{
		"cluster-a": {"a0"},
	})
	defer e.Close(t)

	text := common.FetchMetrics(t, e.EPP.MetricsAddr)
	// llm_d_epp_info is written once at startup (build info), independent of
	// traffic: its presence proves the collectors are registered.
	if !hasFamily(text, "llm_d_epp_info") {
		t.Fatalf("llm_d_epp_info missing: llm-d EPP collectors are not registered\n%s", text)
	}

	// The datalayer error path exports too: with cluster_table endpoints on
	// reserved (unbound) loopback ports, the metrics data source fails to
	// scrape and records poll errors.
	common.WaitFor(t, 20*time.Second, `llm_d_epp_datalayer_poll_errors_total{source_type="metrics-data-source"} present`, func() bool {
		return hasFamily(common.FetchMetrics(t, e.EPP.MetricsAddr), "llm_d_epp_datalayer_poll_errors_total")
	})

	// Regression: the ai-gateway-native families keep exporting.
	if !hasFamily(text, "ai_epp_cell_state") {
		t.Fatalf("ai_epp_cell_state missing (regression)")
	}
}

// TestTC02_PerEndpointQueueSize: the process-wide collector emits one series
// per registered endpoint, labelled by cluster / pool / endpoint, across all
// live cells (multi-cell, single registration).
func TestTC02_PerEndpointQueueSize(t *testing.T) {
	e := common.NewLocalEnv(t, "epp-sc16-tc02", map[string][]string{
		"cluster-a": {"a0", "a1"},
		"cluster-b": {"b0"},
	})
	defer e.Close(t)

	// Local mode registers endpoints from cluster_table but never dials them,
	// so the queue size is 0. The endpoint label is the cluster_table name.
	want := []string{
		`ai_epp_per_endpoint_queue_size{cluster="cluster-a",inference_pool="cluster-a",model_server_endpoint="cluster-a-a0"}`,
		`ai_epp_per_endpoint_queue_size{cluster="cluster-a",inference_pool="cluster-a",model_server_endpoint="cluster-a-a1"}`,
		`ai_epp_per_endpoint_queue_size{cluster="cluster-b",inference_pool="cluster-b",model_server_endpoint="cluster-b-b0"}`,
	}
	common.WaitFor(t, 20*time.Second, "per-endpoint samples present for all endpoints", func() bool {
		text := common.FetchMetrics(t, e.EPP.MetricsAddr)
		for _, p := range want {
			if _, ok := sample(text, p); !ok {
				return false
			}
		}
		return true
	})

	text := common.FetchMetrics(t, e.EPP.MetricsAddr)
	for _, p := range want {
		v, ok := sample(text, p)
		if !ok {
			t.Fatalf("sample absent: %s", p)
		}
		if v != 0 {
			t.Fatalf("%s = %v, want 0 (no backend contacted)", p, v)
		}
	}
}

// TestTC03_PoolAggregateReadyEndpoints: the per-cell flush loop produces
// ready_endpoints as soon as the cell's pool is bound. With no fresh endpoint
// metrics (local mode), it is 0 and the average_*/std_dev_* families stay
// absent (podCount == 0) instead of exporting misleading zeros.
func TestTC03_PoolAggregateReadyEndpoints(t *testing.T) {
	e := common.NewLocalEnv(t, "epp-sc16-tc03", map[string][]string{
		"cluster-a": {"a0"},
		"cluster-b": {"b0"},
	}, common.WithLocalEppArgs("-pool-metrics-flush-interval=200ms"))
	defer e.Close(t)

	readyA := `llm_d_epp_ready_endpoints{name="cluster-a"}`
	readyB := `llm_d_epp_ready_endpoints{name="cluster-b"}`
	common.WaitFor(t, 20*time.Second, "ready_endpoints present for both clusters", func() bool {
		text := common.FetchMetrics(t, e.EPP.MetricsAddr)
		_, okA := sample(text, readyA)
		_, okB := sample(text, readyB)
		return okA && okB
	})

	text := common.FetchMetrics(t, e.EPP.MetricsAddr)
	for _, p := range []string{readyA, readyB} {
		if v, _ := sample(text, p); v != 0 {
			t.Fatalf("%s = %v, want 0 (no fresh endpoint metrics)", p, v)
		}
	}

	// Aggregates require fresh endpoints; they must be absent here.
	for _, m := range []string{
		"llm_d_epp_average_kv_cache_utilization",
		"llm_d_epp_average_queue_size",
		"llm_d_epp_average_running_requests",
		"llm_d_epp_std_dev_kv_cache_utilization",
		"llm_d_epp_std_dev_queue_size",
		"llm_d_epp_std_dev_running_requests",
	} {
		if hasFamily(text, m) {
			t.Fatalf("%s present without fresh endpoints (want absent)", m)
		}
	}
}

// TestTC04_MetricsFlagsAccepted: the new metrics flags are accepted and the
// process stays SERVING with the metrics still exported.
func TestTC04_MetricsFlagsAccepted(t *testing.T) {
	e := common.NewLocalEnv(t, "epp-sc16-tc04", map[string][]string{
		"cluster-a": {"a0"},
	}, common.WithLocalEppArgs(
		"-pool-metrics-flush-interval=200ms",
		"-pool-metrics-staleness-window=1s",
		"-fairness-id-metric-label-limit=5",
	))
	defer e.Close(t)

	e.EPP.WaitHealth(t, "", 30*time.Second)

	if text := common.FetchMetrics(t, e.EPP.MetricsAddr); !hasFamily(text, "llm_d_epp_info") {
		t.Fatalf("llm_d_epp_info missing with custom metrics flags")
	}
	common.WaitFor(t, 20*time.Second, "per-endpoint sample present with custom metrics flags", func() bool {
		_, ok := sample(common.FetchMetrics(t, e.EPP.MetricsAddr),
			`ai_epp_per_endpoint_queue_size{cluster="cluster-a",inference_pool="cluster-a",model_server_endpoint="cluster-a-a0"}`)
		return ok
	})
}

// TestTC05_PoolAggregatesWithFreshMetrics: once the data source scrapes the
// sims' /metrics, endpoints count as fresh, so ready_endpoints reflects them
// and the aggregate families are produced. Requires inference-sim (t.Skip when
// the binary is unavailable).
func TestTC05_PoolAggregatesWithFreshMetrics(t *testing.T) {
	e := common.NewEnv(t, "epp-sc16-tc05", map[string][]string{
		"cluster-a": {"a0", "a1"},
	}, common.WithEppArgs(
		"-pool-metrics-flush-interval=200ms",
		"-pool-metrics-staleness-window=5s",
	))
	defer e.Close(t)

	ready := `llm_d_epp_ready_endpoints{name="cluster-a"}`
	common.WaitFor(t, 30*time.Second, "ready_endpoints counts fresh sim endpoints", func() bool {
		v, _ := sample(common.FetchMetrics(t, e.EPP.MetricsAddr), ready)
		return v >= 1
	})

	common.WaitFor(t, 30*time.Second, "average_kv_cache_utilization produced with fresh endpoints", func() bool {
		return hasFamily(common.FetchMetrics(t, e.EPP.MetricsAddr), "llm_d_epp_average_kv_cache_utilization")
	})
}
