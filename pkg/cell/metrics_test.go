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
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-router/pkg/epp/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/datastore"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
)

// stubDS satisfies datastore.Datastore by embedding the interface; only the two
// methods the collector calls are overridden.
type stubDS struct {
	datastore.Datastore
	pool *datalayer.EndpointPool
	err  error
	pods []fwkdl.Endpoint
}

func (s stubDS) PoolGet() (*datalayer.EndpointPool, error) { return s.pool, s.err }
func (s stubDS) PodList(func(fwkdl.Endpoint) bool) []fwkdl.Endpoint {
	return s.pods
}

func endpoint(namespace, name string, queue int) fwkdl.Endpoint {
	return fwkdl.NewEndpoint(
		&fwkdl.EndpointMetadata{ID: k8stypes.NamespacedName{Namespace: namespace, Name: name}},
		&fwkdl.Metrics{WaitingQueueSize: queue},
	)
}

func TestPerEndpointQueueSizeCollector(t *testing.T) {
	sim := &Cell{
		Key: Key("cluster_epp_sim"),
		ds: stubDS{
			pool: datalayer.NewEndpointPool("ns", "cluster_epp_sim"),
			pods: []fwkdl.Endpoint{
				endpoint("cluster_epp_sim", "127.0.0.1_8981", 5),
				endpoint("cluster_epp_sim", "127.0.0.1_8982", 0),
			},
		},
	}
	real := &Cell{
		Key: Key("cluster_epp_real"),
		ds: stubDS{
			pool: datalayer.NewEndpointPool("ns", "cluster_epp_real"),
			pods: []fwkdl.Endpoint{
				endpoint("cluster_epp_real", "172.19.1.126_8000", 2),
			},
		},
	}
	// PoolGet fails: the cell must be skipped, contributing no samples.
	down := &Cell{
		Key: Key("cluster_down"),
		ds:  stubDS{err: errors.New("pool not synced")},
	}

	collector := NewPerEndpointQueueSizeCollector(func() []*Cell {
		return []*Cell{sim, real, down}
	})

	const expected = `
# HELP ai_epp_per_endpoint_queue_size Current number of requests waiting in the model-server queue, per endpoint, per cluster.
# TYPE ai_epp_per_endpoint_queue_size gauge
ai_epp_per_endpoint_queue_size{cluster="cluster_epp_real",inference_pool="cluster_epp_real",model_server_endpoint="172.19.1.126_8000"} 2
ai_epp_per_endpoint_queue_size{cluster="cluster_epp_sim",inference_pool="cluster_epp_sim",model_server_endpoint="127.0.0.1_8981"} 5
ai_epp_per_endpoint_queue_size{cluster="cluster_epp_sim",inference_pool="cluster_epp_sim",model_server_endpoint="127.0.0.1_8982"} 0
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), "ai_epp_per_endpoint_queue_size"); err != nil {
		t.Fatalf("unexpected metrics:\n%v", err)
	}
}
