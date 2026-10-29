package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	entriesDesc = prometheus.NewDesc(
		"cilium_policy_map_entries",
		"Number of entries in the endpoint's policy BPF map. Only exported for maps at least --entries-min-usage full.",
		[]string{"node", "namespace", "pod", "endpoint_id", "endpoint_type"}, nil)
	maxEntriesDesc = prometheus.NewDesc(
		"cilium_policy_map_max_entries",
		"Maximum number of entries a policy BPF map can hold (bpf-policy-map-max). Every endpoint's map has the same size, so this has no labels.",
		nil, nil)
	policyMapsDesc = prometheus.NewDesc(
		"cilium_policy_maps",
		"Number of endpoint policy BPF maps read on the node, whether or not their entries are exported.",
		[]string{"node"}, nil)
	policyMapsOverDesc = prometheus.NewDesc(
		"cilium_policy_maps_over_usage_threshold",
		"Number of endpoint policy BPF maps on the node at least --entries-min-usage full, i.e. those exported in cilium_policy_map_entries.",
		[]string{"node"}, nil)
	apiUpDesc = prometheus.NewDesc(
		"cilium_custom_node_exporter_cilium_api_up",
		"Whether the endpoint list from the cilium-agent API is current (1) or the agent could not be reached (0).",
		[]string{"node"}, nil)
	durationDesc = prometheus.NewDesc(
		"cilium_custom_node_exporter_collect_duration_seconds",
		"Time the last pass over the policy maps took.",
		[]string{"node"}, nil)
	lastCollectDesc = prometheus.NewDesc(
		"cilium_custom_node_exporter_last_collect_timestamp_seconds",
		"Unix time at which the last pass over the policy maps finished.",
		[]string{"node"}, nil)
)

// policyMapCollector reads every endpoint policy map on the node on its own
// schedule and serves the result of the last pass, so a scrape never touches
// the BPF maps or the cilium-agent API.
type policyMapCollector struct {
	node       string
	mapDir     string
	endpoints  *endpointCache
	apiTimeout time.Duration
	// minUsage is the fraction of max_entries a map must reach before its
	// entry count is exported. 0 exports every map.
	minUsage float64

	errors *prometheus.CounterVec
	last   atomic.Pointer[snapshot]
}

// snapshot is the outcome of one pass over the policy maps.
type snapshot struct {
	maps       []mapSample
	maxEntries uint32 // 0 when no map could be read
	apiUp      bool
	duration   time.Duration
	finishedAt time.Time
}

type mapSample struct {
	endpointID uint16
	endpoint   endpointInfo
	entries    int
	maxEntries uint32
}

func newPolicyMapCollector(node, mapDir string, endpoints *endpointCache, apiTimeout time.Duration, minUsage float64) *policyMapCollector {
	errs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "cilium_custom_node_exporter_errors_total",
		Help:        "Errors while collecting, by stage (list_maps, read_map, cilium_api).",
		ConstLabels: prometheus.Labels{"node": node},
	}, []string{"stage"})
	for _, stage := range []string{"list_maps", "read_map", "cilium_api"} {
		errs.WithLabelValues(stage)
	}
	return &policyMapCollector{
		node:       node,
		mapDir:     mapDir,
		endpoints:  endpoints,
		apiTimeout: apiTimeout,
		minUsage:   minUsage,
		errors:     errs,
	}
}

// run refreshes the snapshot every interval until ctx is done.
func (c *policyMapCollector) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

// refresh reads every policy map and replaces the snapshot served to scrapes.
func (c *policyMapCollector) refresh(ctx context.Context) {
	start := time.Now()

	maps, err := listPolicyMaps(c.mapDir)
	if err != nil {
		slog.Error("Listing policy maps", "dir", c.mapDir, "err", err)
		c.errors.WithLabelValues("list_maps").Inc()
	}

	ids := make([]uint16, len(maps))
	for i, pm := range maps {
		ids[i] = pm.EndpointID
	}
	apiCtx, cancel := context.WithTimeout(ctx, c.apiTimeout)
	eps, err := c.endpoints.lookup(apiCtx, ids)
	cancel()
	if err != nil {
		slog.Warn("Listing endpoints from cilium-agent, using last known pod names", "err", err)
		c.errors.WithLabelValues("cilium_api").Inc()
	}

	s := &snapshot{apiUp: err == nil, maps: make([]mapSample, 0, len(maps))}
	for _, pm := range maps {
		stats, err := readPolicyMap(pm.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue // the endpoint went away during the pass
		}
		if err != nil {
			slog.Warn("Reading policy map", "path", pm.Path, "err", err)
			c.errors.WithLabelValues("read_map").Inc()
			continue
		}
		ep, ok := eps[pm.EndpointID]
		if !ok {
			ep = endpointInfo{Type: "unknown"}
		}
		s.maps = append(s.maps, mapSample{endpointID: pm.EndpointID, endpoint: ep, entries: stats.Entries, maxEntries: stats.MaxEntries})
		s.maxEntries = max(s.maxEntries, stats.MaxEntries)
	}
	s.duration = time.Since(start)
	s.finishedAt = time.Now()
	c.last.Store(s)
}

func (c *policyMapCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- entriesDesc
	ch <- maxEntriesDesc
	ch <- policyMapsDesc
	ch <- policyMapsOverDesc
	ch <- apiUpDesc
	ch <- durationDesc
	ch <- lastCollectDesc
	c.errors.Describe(ch)
}

// Collect serves the last snapshot. Until the first pass finishes, only the
// error counters are reported.
func (c *policyMapCollector) Collect(ch chan<- prometheus.Metric) {
	c.errors.Collect(ch)
	s := c.last.Load()
	if s == nil {
		return
	}

	over := 0
	for _, m := range s.maps {
		if !c.overThreshold(m) {
			continue
		}
		over++
		ch <- prometheus.MustNewConstMetric(entriesDesc, prometheus.GaugeValue, float64(m.entries),
			c.node, m.endpoint.Namespace, m.endpoint.Pod, strconv.Itoa(int(m.endpointID)), m.endpoint.Type)
	}
	ch <- prometheus.MustNewConstMetric(policyMapsDesc, prometheus.GaugeValue, float64(len(s.maps)), c.node)
	ch <- prometheus.MustNewConstMetric(policyMapsOverDesc, prometheus.GaugeValue, float64(over), c.node)
	if s.maxEntries > 0 {
		ch <- prometheus.MustNewConstMetric(maxEntriesDesc, prometheus.GaugeValue, float64(s.maxEntries))
	}
	apiUp := 0.0
	if s.apiUp {
		apiUp = 1
	}
	ch <- prometheus.MustNewConstMetric(apiUpDesc, prometheus.GaugeValue, apiUp, c.node)
	ch <- prometheus.MustNewConstMetric(durationDesc, prometheus.GaugeValue, s.duration.Seconds(), c.node)
	ch <- prometheus.MustNewConstMetric(lastCollectDesc, prometheus.GaugeValue, float64(s.finishedAt.UnixNano())/1e9, c.node)
}

// overThreshold reports whether a map is full enough for its entry count to be
// exported. Only maps near their limit matter for alerting, and leaving the
// rest out keeps one series per endpoint off Prometheus.
func (c *policyMapCollector) overThreshold(m mapSample) bool {
	return float64(m.entries) >= c.minUsage*float64(m.maxEntries)
}
