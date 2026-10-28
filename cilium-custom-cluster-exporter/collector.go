package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/tools/cache"
)

var (
	totalDesc = prometheus.NewDesc(
		"cilium_identity_total",
		"Number of CiliumIdentity objects in the cluster.",
		nil, nil)
	perNamespaceDesc = prometheus.NewDesc(
		"cilium_identity_count",
		"Number of CiliumIdentity objects per namespace (from the k8s:io.kubernetes.pod.namespace security label; empty for identities without one).",
		[]string{"namespace"}, nil)
	distinctValuesDesc = prometheus.NewDesc(
		"cilium_identity_label_distinct_values",
		"Number of distinct values a security label key takes across the CiliumIdentity objects of a namespace.",
		[]string{"namespace", "key"}, nil)
	syncedDesc = prometheus.NewDesc(
		"cilium_custom_cluster_exporter_cache_synced",
		"Whether the CiliumIdentity cache has finished its initial list from the API server (1) or not (0). No identity metrics are exported until it has.",
		nil, nil)
	durationDesc = prometheus.NewDesc(
		"cilium_custom_cluster_exporter_collect_duration_seconds",
		"Time the last pass over the cached CiliumIdentity objects took.",
		nil, nil)
	lastCollectDesc = prometheus.NewDesc(
		"cilium_custom_cluster_exporter_last_collect_timestamp_seconds",
		"Unix time at which the last pass over the cached CiliumIdentity objects finished.",
		nil, nil)
)

// identityCollector aggregates the CiliumIdentity objects held by an informer
// on its own schedule and serves the result of the last pass, so a scrape
// never walks the cache or calls the API server.
type identityCollector struct {
	store  cache.Store
	synced cache.InformerSynced

	last atomic.Pointer[clusterSnapshot]
}

type clusterSnapshot struct {
	agg        aggregate
	duration   time.Duration
	finishedAt time.Time
}

func newIdentityCollector(store cache.Store, synced cache.InformerSynced) *identityCollector {
	return &identityCollector{store: store, synced: synced}
}

// run refreshes the snapshot every interval until ctx is done.
func (c *identityCollector) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh()
		}
	}
}

// refresh aggregates the cache and replaces the snapshot served to scrapes.
// Before the cache has synced it does nothing, so a half-listed cache is never
// reported as the number of identities.
func (c *identityCollector) refresh() {
	if !c.synced() {
		return
	}
	start := time.Now()
	objs := c.store.List()
	ids := make([]identity, 0, len(objs))
	for _, obj := range objs {
		if id, ok := identityFromObject(obj); ok {
			ids = append(ids, id)
		}
	}
	s := &clusterSnapshot{agg: aggregateIdentities(ids)}
	s.duration = time.Since(start)
	s.finishedAt = time.Now()
	c.last.Store(s)
}

func (c *identityCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- totalDesc
	ch <- perNamespaceDesc
	ch <- distinctValuesDesc
	ch <- syncedDesc
	ch <- durationDesc
	ch <- lastCollectDesc
}

// Collect serves the last snapshot. Until the first pass finishes, only
// cilium_custom_cluster_exporter_cache_synced is reported.
func (c *identityCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.last.Load()
	synced := 0.0
	if s != nil {
		synced = 1
	}
	ch <- prometheus.MustNewConstMetric(syncedDesc, prometheus.GaugeValue, synced)
	if s == nil {
		return
	}

	a := s.agg
	ch <- prometheus.MustNewConstMetric(totalDesc, prometheus.GaugeValue, float64(a.total))
	for ns, n := range a.perNS {
		ch <- prometheus.MustNewConstMetric(perNamespaceDesc, prometheus.GaugeValue, float64(n), ns)
	}
	for k, n := range a.distinct {
		ch <- prometheus.MustNewConstMetric(distinctValuesDesc, prometheus.GaugeValue, float64(n), k.namespace, k.key)
	}
	ch <- prometheus.MustNewConstMetric(durationDesc, prometheus.GaugeValue, s.duration.Seconds())
	ch <- prometheus.MustNewConstMetric(lastCollectDesc, prometheus.GaugeValue, float64(s.finishedAt.UnixNano())/1e9)
}
