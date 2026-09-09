package server

import (
	"bytes"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/pkg/queue"
	"github.com/ustclug/rsync-proxy/pkg/state"
)

func TestAggregateRetainsCountersWithoutRetiredGauges(t *testing.T) {
	store, err := state.Open(t.TempDir())
	require.NoError(t, err)
	defer store.Close()
	makeGeneration := func(id string) *Server {
		s := New()
		s.Shared, s.Generation = store, id
		s.upstreams = []upstreamConfig{{Name: "u"}}
		s.upstreamQueues["u"] = queue.New(5, 2)
		s.connInfo.Store(uint32(1), &ConnInfo{Index: 1, Generation: id, Module: "foo", Upstream: "u"})
		s.activeConnCount.Store(1)
		s.acceptedConnCount.Store(3)
		s.completedConnCount.Store(2)
		s.sentBytesTotal.Store(42)
		require.NoError(t, store.Register(state.Generation{ID: id, Version: id}))
		return s
	}
	a, b := makeGeneration("a"), makeGeneration("b")
	limits := []state.Limit{{Name: "u", Active: 5, Queued: 2}}
	require.NoError(t, store.Activate("a", "", limits))
	require.NoError(t, a.PublishSnapshot())
	require.NoError(t, store.Activate("b", "a", limits))
	for _, retired := range []bool{false, true} {
		if retired {
			require.NoError(t, store.Finish("a"))
		}
		var output bytes.Buffer
		require.NoError(t, b.AggregateMetrics(&output))
		parser := expfmt.NewTextParser(model.LegacyValidation)
		metrics, err := parser.TextToMetricFamilies(&output)
		require.NoError(t, err)
		require.InDelta(t, float64(6), metrics["rsync_proxy_accepted_connections_total"].Metric[0].Counter.GetValue(), 0.001)
		require.InDelta(t, float64(84), metrics["rsync_proxy_sent_bytes_total"].Metric[0].Counter.GetValue(), 0.001)
		require.InDelta(t, float64(5), metrics["rsync_proxy_queue_active_max"].Metric[0].Gauge.GetValue(), 0.001)
		want := 2
		if retired {
			want = 1
		}
		require.InDelta(t, float64(want), metrics["rsync_proxy_active_connections"].Metric[0].Gauge.GetValue(), 0.001)
		require.Len(t, metrics["rsync_proxy_connection_sent_bytes"].Metric, want)
		status, err := b.Status()
		require.NoError(t, err)
		require.Equal(t, want, status.Count)
	}
}
