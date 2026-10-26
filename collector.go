package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	mapLabels = []string{"node", "namespace", "pod", "endpoint_id", "endpoint_type"}

	entriesDesc = prometheus.NewDesc(
		"cilium_policy_map_entries",
		"Number of entries in the endpoint's policy BPF map.",
		mapLabels, nil)
	maxEntriesDesc = prometheus.NewDesc(
		"cilium_policy_map_max_entries",
		"Maximum number of entries the endpoint's policy BPF map can hold.",
		mapLabels, nil)
	apiUpDesc = prometheus.NewDesc(
		"cilium_custom_exporter_cilium_api_up",
		"Whether the endpoint list from the cilium-agent API is current (1) or the agent could not be reached (0).",
		[]string{"node"}, nil)
	durationDesc = prometheus.NewDesc(
		"cilium_custom_exporter_collect_duration_seconds",
		"Time spent reading the policy maps for this scrape.",
		[]string{"node"}, nil)
)

// policyMapCollector reads every endpoint policy map on the node at scrape
// time.
type policyMapCollector struct {
	node       string
	mapDir     string
	endpoints  *endpointCache
	apiTimeout time.Duration

	errors *prometheus.CounterVec

	// mu serializes scrapes so concurrent scrapers do not walk the maps
	// twice at the same time.
	mu sync.Mutex
}

func newPolicyMapCollector(node, mapDir string, endpoints *endpointCache, apiTimeout time.Duration) *policyMapCollector {
	errs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "cilium_custom_exporter_errors_total",
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
		errors:     errs,
	}
}

func (c *policyMapCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- entriesDesc
	ch <- maxEntriesDesc
	ch <- apiUpDesc
	ch <- durationDesc
	c.errors.Describe(ch)
}

func (c *policyMapCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	ctx, cancel := context.WithTimeout(context.Background(), c.apiTimeout)
	eps, err := c.endpoints.lookup(ctx, ids)
	cancel()
	apiUp := 1.0
	if err != nil {
		apiUp = 0
		slog.Warn("Listing endpoints from cilium-agent, using last known pod names", "err", err)
		c.errors.WithLabelValues("cilium_api").Inc()
	}
	ch <- prometheus.MustNewConstMetric(apiUpDesc, prometheus.GaugeValue, apiUp, c.node)

	for _, pm := range maps {
		stats, err := readPolicyMap(pm.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue // the endpoint went away during the scrape
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
		lv := []string{c.node, ep.Namespace, ep.Pod, strconv.Itoa(int(pm.EndpointID)), ep.Type}
		ch <- prometheus.MustNewConstMetric(entriesDesc, prometheus.GaugeValue, float64(stats.Entries), lv...)
		ch <- prometheus.MustNewConstMetric(maxEntriesDesc, prometheus.GaugeValue, float64(stats.MaxEntries), lv...)
	}

	ch <- prometheus.MustNewConstMetric(durationDesc, prometheus.GaugeValue, time.Since(start).Seconds(), c.node)
	c.errors.Collect(ch)
}
