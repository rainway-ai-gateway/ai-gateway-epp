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

package cell

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/llm-d/llm-d-router/pkg/epp/datastore"
)

var (
	engineReloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "ai_epp",
		Name:      "engine_reloads_total",
		Help:      "Engine compile/swap attempts by cluster and result.",
	}, []string{"cluster", "result"})

	engineCurrentVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "ai_epp",
		Name:      "engine_current_version",
		Help:      "Content hash of the currently active engine config, as a labeled gauge value of 1.",
	}, []string{"cluster", "version"})

	cellStateGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "ai_epp",
		Name:      "cell_state",
		Help:      "Cell state by cluster and state; 1 for the current combination.",
	}, []string{"cluster", "state"})

	drainDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "ai_epp",
		Name:      "drain_duration_seconds",
		Help:      "Time to drain a replaced engine by cluster.",
		Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12),
	}, []string{"cluster"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(engineReloads, engineCurrentVersion, cellStateGauge, drainDuration)
}

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
