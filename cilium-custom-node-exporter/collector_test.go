package main

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCollectBeforeFirstPass(t *testing.T) {
	c := newPolicyMapCollector("node-a", t.TempDir(), &endpointCache{client: newCiliumClient(filepath.Join(t.TempDir(), "cilium.sock"))}, time.Second, 0)
	if n := testutil.CollectAndCount(c, "cilium_policy_map_entries", "cilium_custom_node_exporter_last_collect_timestamp_seconds"); n != 0 {
		t.Errorf("collected %d snapshot metrics before the first pass, want 0", n)
	}
	if n := testutil.CollectAndCount(c, "cilium_custom_node_exporter_errors_total"); n != 3 {
		t.Errorf("collected %d error counters, want 3", n)
	}
}

// TestRun checks that run refreshes the snapshot on its own, stops with its
// context, and that scrapes in between do not ask cilium-agent for anything.
func TestRun(t *testing.T) {
	var calls atomic.Int32
	sock := serveFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte("[]"))
	})
	c := newPolicyMapCollector("node-a", t.TempDir(), &endpointCache{client: newCiliumClient(sock)}, time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.run(ctx, 10*time.Millisecond)
		close(done)
	}()

	seen := make(map[*snapshot]bool)
	for deadline := time.Now().Add(5 * time.Second); len(seen) < 2 && time.Now().Before(deadline); {
		if s := c.last.Load(); s != nil {
			seen[s] = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(seen) < 2 {
		t.Fatalf("run produced %d snapshots in 5s, want at least 2", len(seen))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after its context was cancelled")
	}

	n := calls.Load()
	for i := 0; i < 5; i++ {
		testutil.CollectAndCount(c)
	}
	if got := calls.Load(); got != n {
		t.Errorf("scrapes called the agent API %d times, want 0", got-n)
	}
}

// TestEntriesMinUsage checks that only maps at or over the usage threshold get
// an entries series, while the map counts and max_entries cover every map.
func TestEntriesMinUsage(t *testing.T) {
	c := newPolicyMapCollector("node-a", t.TempDir(), &endpointCache{client: newCiliumClient(filepath.Join(t.TempDir(), "cilium.sock"))}, time.Second, 0.6)
	c.last.Store(&snapshot{
		maxEntries: 100,
		maps: []mapSample{
			{endpointID: 1, endpoint: endpointInfo{Namespace: "a", Pod: "low", Type: "pod"}, entries: 59, maxEntries: 100},
			{endpointID: 2, endpoint: endpointInfo{Namespace: "a", Pod: "edge", Type: "pod"}, entries: 60, maxEntries: 100},
			{endpointID: 3, endpoint: endpointInfo{Namespace: "a", Pod: "full", Type: "pod"}, entries: 100, maxEntries: 100},
		},
	})
	want := `
# HELP cilium_policy_map_entries Number of entries in the endpoint's policy BPF map. Only exported for maps at least --entries-min-usage full.
# TYPE cilium_policy_map_entries gauge
cilium_policy_map_entries{endpoint_id="2",endpoint_type="pod",namespace="a",node="node-a",pod="edge"} 60
cilium_policy_map_entries{endpoint_id="3",endpoint_type="pod",namespace="a",node="node-a",pod="full"} 100
# HELP cilium_policy_map_max_entries Maximum number of entries a policy BPF map can hold (bpf-policy-map-max). Every endpoint's map has the same size, so this has no labels.
# TYPE cilium_policy_map_max_entries gauge
cilium_policy_map_max_entries 100
# HELP cilium_policy_maps Number of endpoint policy BPF maps read on the node, whether or not their entries are exported.
# TYPE cilium_policy_maps gauge
cilium_policy_maps{node="node-a"} 3
# HELP cilium_policy_maps_over_usage_threshold Number of endpoint policy BPF maps on the node at least --entries-min-usage full, i.e. those exported in cilium_policy_map_entries.
# TYPE cilium_policy_maps_over_usage_threshold gauge
cilium_policy_maps_over_usage_threshold{node="node-a"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"cilium_policy_map_entries", "cilium_policy_map_max_entries", "cilium_policy_maps", "cilium_policy_maps_over_usage_threshold"); err != nil {
		t.Error(err)
	}

	// 0 exports every map, including empty ones.
	c.minUsage = 0
	if n := testutil.CollectAndCount(c, "cilium_policy_map_entries"); n != 3 {
		t.Errorf("%d entries series with threshold 0, want 3", n)
	}
}
