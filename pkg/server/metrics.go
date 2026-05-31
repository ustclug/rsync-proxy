package server

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/queue"
)

func prometheusEscapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func prometheusLabelValueOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func prometheusLabels(index uint32, module, upstream string) string {
	return fmt.Sprintf(
		`index="%d",module="%s",upstream="%s"`,
		index,
		prometheusEscapeLabelValue(prometheusLabelValueOrUnknown(module)),
		prometheusEscapeLabelValue(prometheusLabelValueOrUnknown(upstream)),
	)
}

type prometheusConnectionGroup struct {
	module   string
	upstream string
}

func (s *Server) writePrometheusMetrics(w io.Writer, now time.Time) {
	connections := s.ListConnectionInfo()

	s.reloadLock.RLock()
	upstreams := make([]upstreamConfig, len(s.upstreams))
	copy(upstreams, s.upstreams)
	queues := make(map[string]*queue.Queue, len(s.upstreamQueues))
	for k, v := range s.upstreamQueues {
		queues[k] = v
	}
	s.reloadLock.RUnlock()

	sort.Slice(upstreams, func(i, j int) bool {
		return upstreams[i].Name < upstreams[j].Name
	})

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_queued_connections Current queued rsync proxy connections per upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_queued_connections gauge")
	for _, u := range upstreams {
		if q, ok := queues[u.Name]; ok {
			_, _ = fmt.Fprintf(w, "rsync_proxy_queued_connections{upstream=\"%s\"} %d\n",
				prometheusEscapeLabelValue(u.Name), q.QueuedLen())
		}
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_queue_active_max Configured max active connections per upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_queue_active_max gauge")
	for _, u := range upstreams {
		if q, ok := queues[u.Name]; ok {
			_, _ = fmt.Fprintf(w, "rsync_proxy_queue_active_max{upstream=\"%s\"} %d\n",
				prometheusEscapeLabelValue(u.Name), q.GetMax())
		}
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_queue_queued_max Configured max queued connections per upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_queue_queued_max gauge")
	for _, u := range upstreams {
		if q, ok := queues[u.Name]; ok {
			_, _ = fmt.Fprintf(w, "rsync_proxy_queue_queued_max{upstream=\"%s\"} %d\n",
				prometheusEscapeLabelValue(u.Name), q.GetMaxQueued())
		}
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_queue_full_rejected_total Total connections rejected due to queue full per upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_queue_full_rejected_total counter")
	for _, u := range upstreams {
		c := s.getUpstreamCounters(u.Name)
		_, _ = fmt.Fprintf(w, "rsync_proxy_queue_full_rejected_total{upstream=\"%s\"} %d\n",
			prometheusEscapeLabelValue(u.Name), c.queueFull.Load())
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_upstream_dial_errors_total Total upstream dial failures per upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_upstream_dial_errors_total counter")
	for _, u := range upstreams {
		c := s.getUpstreamCounters(u.Name)
		_, _ = fmt.Fprintf(w, "rsync_proxy_upstream_dial_errors_total{upstream=\"%s\"} %d\n",
			prometheusEscapeLabelValue(u.Name), c.dialError.Load())
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_per_ip_rejected_total Total connections rejected by the per-IP per-upstream concurrency cap.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_per_ip_rejected_total counter")
	for _, u := range upstreams {
		c := s.getUpstreamCounters(u.Name)
		_, _ = fmt.Fprintf(w, "rsync_proxy_per_ip_rejected_total{upstream=\"%s\"} %d\n",
			prometheusEscapeLabelValue(u.Name), c.perIPRejected.Load())
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_unknown_module_requests_total Total requests for unknown modules.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_unknown_module_requests_total counter")
	_, _ = fmt.Fprintf(w, "rsync_proxy_unknown_module_requests_total %d\n", s.unknownModuleCount.Load())

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_active_connections Current active rsync proxy connections.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_active_connections gauge")
	_, _ = fmt.Fprintf(w, "rsync_proxy_active_connections %d\n", s.GetActiveConnectionCount())

	connectionCounts := make(map[prometheusConnectionGroup]int)
	for _, conn := range connections {
		snapshot := conn.snapshot()
		key := prometheusConnectionGroup{
			module:   prometheusLabelValueOrUnknown(snapshot.Module),
			upstream: prometheusLabelValueOrUnknown(snapshot.Upstream),
		}
		connectionCounts[key]++
	}

	keys := make([]prometheusConnectionGroup, 0, len(connectionCounts))
	for key := range connectionCounts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].module != keys[j].module {
			return keys[i].module < keys[j].module
		}
		return keys[i].upstream < keys[j].upstream
	})

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_active_connections_by_module Current active rsync proxy connections by module and upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_active_connections_by_module gauge")
	for _, key := range keys {
		module := prometheusEscapeLabelValue(key.module)
		upstream := prometheusEscapeLabelValue(key.upstream)
		_, _ = fmt.Fprintf(w, "rsync_proxy_active_connections_by_module{module=\"%s\",upstream=\"%s\"} %d\n", module, upstream, connectionCounts[key])
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_connection_sent_bytes Bytes sent to clients for active connections.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_connection_sent_bytes gauge")
	for _, conn := range connections {
		snapshot := conn.snapshot()
		_, _ = fmt.Fprintf(w, "rsync_proxy_connection_sent_bytes{%s} %d\n", prometheusLabels(snapshot.Index, snapshot.Module, snapshot.Upstream), snapshot.SentBytes)
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_connection_received_bytes Bytes received from clients for active connections.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_connection_received_bytes gauge")
	for _, conn := range connections {
		snapshot := conn.snapshot()
		_, _ = fmt.Fprintf(w, "rsync_proxy_connection_received_bytes{%s} %d\n", prometheusLabels(snapshot.Index, snapshot.Module, snapshot.Upstream), snapshot.ReceivedBytes)
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_connection_connected_timestamp_seconds Unix timestamp when active connections were established.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_connection_connected_timestamp_seconds gauge")
	for _, conn := range connections {
		snapshot := conn.snapshot()
		_, _ = fmt.Fprintf(w, "rsync_proxy_connection_connected_timestamp_seconds{%s} %d\n", prometheusLabels(snapshot.Index, snapshot.Module, snapshot.Upstream), snapshot.ConnectedAt.Unix())
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_connection_duration_seconds Current duration of active connections.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_connection_duration_seconds gauge")
	for _, conn := range connections {
		snapshot := conn.snapshot()
		duration := now.Sub(snapshot.ConnectedAt).Seconds()
		if duration < 0 {
			duration = 0
		}
		_, _ = fmt.Fprintf(w, "rsync_proxy_connection_duration_seconds{%s} %.3f\n", prometheusLabels(snapshot.Index, snapshot.Module, snapshot.Upstream), duration)
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_accepted_connections_total Total accepted connections since start.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_accepted_connections_total counter")
	_, _ = fmt.Fprintf(w, "rsync_proxy_accepted_connections_total %d\n", s.acceptedConnCount.Load())

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_completed_connections_total Total completed connections since start.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_completed_connections_total counter")
	_, _ = fmt.Fprintf(w, "rsync_proxy_completed_connections_total %d\n", s.completedConnCount.Load())

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_sent_bytes_total Total bytes sent to clients since start.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_sent_bytes_total counter")
	_, _ = fmt.Fprintf(w, "rsync_proxy_sent_bytes_total %d\n", s.sentBytesTotal.Load())

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_received_bytes_total Total bytes received from clients since start.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_received_bytes_total counter")
	_, _ = fmt.Fprintf(w, "rsync_proxy_received_bytes_total %d\n", s.recvBytesTotal.Load())

	// Per-(module, upstream) lifetime counters. We collect a sorted slice of
	// keys so that the text output is stable across scrapes.
	type moduleStat struct {
		key       moduleUpstreamKey
		completed uint64
		sentBytes uint64
		recvBytes uint64
	}
	var moduleStats []moduleStat
	s.moduleCounters.Range(func(k, v any) bool {
		key := k.(moduleUpstreamKey)
		c := v.(*moduleCounters)
		moduleStats = append(moduleStats, moduleStat{
			key:       key,
			completed: c.completed.Load(),
			sentBytes: c.sentBytes.Load(),
			recvBytes: c.recvBytes.Load(),
		})
		return true
	})
	sort.Slice(moduleStats, func(i, j int) bool {
		if moduleStats[i].key.module != moduleStats[j].key.module {
			return moduleStats[i].key.module < moduleStats[j].key.module
		}
		return moduleStats[i].key.upstream < moduleStats[j].key.upstream
	})

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_module_completed_connections_total Total completed connections by module and upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_module_completed_connections_total counter")
	for _, m := range moduleStats {
		_, _ = fmt.Fprintf(w, "rsync_proxy_module_completed_connections_total{module=\"%s\",upstream=\"%s\"} %d\n",
			prometheusEscapeLabelValue(m.key.module),
			prometheusEscapeLabelValue(m.key.upstream),
			m.completed)
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_module_sent_bytes_total Total bytes sent to clients by module and upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_module_sent_bytes_total counter")
	for _, m := range moduleStats {
		_, _ = fmt.Fprintf(w, "rsync_proxy_module_sent_bytes_total{module=\"%s\",upstream=\"%s\"} %d\n",
			prometheusEscapeLabelValue(m.key.module),
			prometheusEscapeLabelValue(m.key.upstream),
			m.sentBytes)
	}

	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_module_received_bytes_total Total bytes received from clients by module and upstream.")
	_, _ = fmt.Fprintln(w, "# TYPE rsync_proxy_module_received_bytes_total counter")
	for _, m := range moduleStats {
		_, _ = fmt.Fprintf(w, "rsync_proxy_module_received_bytes_total{module=\"%s\",upstream=\"%s\"} %d\n",
			prometheusEscapeLabelValue(m.key.module),
			prometheusEscapeLabelValue(m.key.upstream),
			m.recvBytes)
	}
}
