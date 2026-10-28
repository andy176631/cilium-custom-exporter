package main

import (
	"context"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCollectBeforeFirstPass(t *testing.T) {
	c := newPolicyMapCollector("node-a", t.TempDir(), &endpointCache{client: newCiliumClient(filepath.Join(t.TempDir(), "cilium.sock"))}, time.Second)
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
	c := newPolicyMapCollector("node-a", t.TempDir(), &endpointCache{client: newCiliumClient(sock)}, time.Second)

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
