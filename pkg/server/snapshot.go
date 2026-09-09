package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/ustclug/rsync-proxy/pkg/state"
)

type generationSnapshot struct {
	Updated     time.Time          `json:"updated"`
	Connections []connInfoSnapshot `json:"connections"`
	Count       int64              `json:"count"`
	Metrics     string             `json:"metrics"`
}

type Status struct {
	Count       int                `json:"count"`
	Connections []connInfoSnapshot `json:"connections"`
	Generations []state.Generation `json:"generations,omitempty"`
}

func (s *Server) snapshot() generationSnapshot {
	result := generationSnapshot{Updated: time.Now(), Count: s.GetActiveConnectionCount(), Connections: make([]connInfoSnapshot, 0)}
	for _, c := range s.ListConnectionInfo() {
		result.Connections = append(result.Connections, c.snapshot())
	}
	var b bytes.Buffer
	s.writePrometheusMetrics(&b, result.Updated)
	result.Metrics = b.String()
	return result
}

func (s *Server) PublishSnapshot() error {
	b, err := json.Marshal(s.snapshot())
	if err != nil {
		return err
	}
	return s.Shared.Publish(s.Generation, b)
}

func (s *Server) Status() (Status, error) {
	result := Status{Connections: make([]connInfoSnapshot, 0)}
	if s.Shared == nil {
		result.Connections = s.snapshot().Connections
	} else {
		generations, err := s.Shared.Generations()
		if err != nil {
			return result, err
		}
		result.Generations = generations
		for _, g := range generations {
			if g.State == state.Exited || g.State == state.Starting {
				continue
			}
			var snap generationSnapshot
			if g.ID == s.Generation {
				snap = s.snapshot()
			} else if len(g.Snapshot) > 0 {
				if err := json.Unmarshal(g.Snapshot, &snap); err != nil {
					return result, err
				}
			}
			result.Connections = append(result.Connections, snap.Connections...)
		}
	}
	result.Count = len(result.Connections)
	return result, nil
}

// Aggregate only proxy metrics. Runtime/process metrics belong to the serving
// process. Retired generations contribute counters, never stale live gauges.
func (s *Server) AggregateMetrics(w io.Writer) error {
	gens, err := s.Shared.Generations()
	if err != nil {
		return err
	}
	families := make(map[string]*dto.MetricFamily)
	samples := make(map[string]*dto.Metric)
	skip := map[string]bool{"rsync_proxy_queued_connections": true, "rsync_proxy_queue_active_max": true, "rsync_proxy_queue_queued_max": true}
	for _, g := range gens {
		var snap generationSnapshot
		if g.ID == s.Generation {
			snap = s.snapshot()
		} else if len(g.Snapshot) > 0 {
			if err := json.Unmarshal(g.Snapshot, &snap); err != nil {
				return err
			}
		}
		parser := expfmt.NewTextParser(model.LegacyValidation)
		parsed, err := parser.TextToMetricFamilies(strings.NewReader(snap.Metrics))
		if err != nil {
			return err
		}
		for name, f := range parsed {
			if skip[name] || (g.State == state.Exited && f.GetType() != dto.MetricType_COUNTER) {
				continue
			}
			dst := families[name]
			if dst == nil {
				dst = &dto.MetricFamily{Name: f.Name, Help: f.Help, Type: f.Type}
				families[name] = dst
			}
			for _, m := range f.Metric {
				sort.Slice(m.Label, func(i, j int) bool { return m.Label[i].GetName() < m.Label[j].GetName() })
				labels, _ := json.Marshal(m.Label)
				key := name + string(labels)
				if old := samples[key]; old != nil {
					if m.Counter != nil {
						v := old.Counter.GetValue() + m.Counter.GetValue()
						old.Counter.Value = &v
					}
					if m.Gauge != nil {
						v := old.Gauge.GetValue() + m.Gauge.GetValue()
						old.Gauge.Value = &v
					}
				} else {
					samples[key] = m
					dst.Metric = append(dst.Metric, m)
				}
			}
		}
	}
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := expfmt.MetricFamilyToText(w, families[name]); err != nil {
			return err
		}
	}
	queues, err := s.Shared.Queues()
	if err != nil {
		return err
	}
	for _, metric := range []struct {
		name, help string
		value      func(state.QueueInfo) int
	}{
		{"rsync_proxy_queued_connections", "Current queued rsync proxy connections per upstream.", func(q state.QueueInfo) int { return q.QueuedCount }},
		{"rsync_proxy_queue_active_max", "Configured max active connections per upstream.", func(q state.QueueInfo) int { return q.Active }},
		{"rsync_proxy_queue_queued_max", "Configured max queued connections per upstream.", func(q state.QueueInfo) int { return q.Queued }},
	} {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", metric.name, metric.help, metric.name)
		for _, q := range queues {
			_, _ = fmt.Fprintf(w, "%s{upstream=\"%s\"} %d\n", metric.name, prometheusEscapeLabelValue(q.Name), metric.value(q))
		}
	}
	_, _ = fmt.Fprintln(w, "# HELP rsync_proxy_generation_info Process generation metadata.\n# TYPE rsync_proxy_generation_info gauge")
	for _, g := range gens {
		if g.State == state.Exited {
			continue
		}
		_, _ = fmt.Fprintf(w, "rsync_proxy_generation_info{generation=\"%s\",version=\"%s\",state=\"%s\",pid=\"%d\"} 1\n", prometheusEscapeLabelValue(g.ID), prometheusEscapeLabelValue(g.Version), g.State, g.PID)
	}
	return nil
}
