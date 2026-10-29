package main

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/sys/unix"
)

func TestListPolicyMaps(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"cilium_policy_00812",
		"cilium_policy_01504",
		"cilium_policy_812",           // same endpoint, first one wins
		"cilium_policy_00042:pending", // half-written pin
		"cilium_call_policy",
		"cilium_egresscall_policy",
		"cilium_policy_99999", // not a valid endpoint ID
		"cilium_ct4_global",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := listPolicyMaps(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []policyMapFile{
		{EndpointID: 812, Path: filepath.Join(dir, "cilium_policy_00812")},
		{EndpointID: 1504, Path: filepath.Join(dir, "cilium_policy_01504")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listPolicyMaps() = %+v, want %+v", got, want)
	}
}

// The specs below copy the policy maps cilium-agent creates: a hash map in
// 1.13 and an LPM trie in 1.14 (struct policy_key / policy_entry in
// bpf/lib/common.h).
var (
	policyMapSpec113 = &ebpf.MapSpec{
		Type:       ebpf.Hash,
		KeySize:    8, // sec_label, dport, protocol, egress
		ValueSize:  24,
		MaxEntries: 16384,
	}
	policyMapSpec114 = &ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    12, // prefixlen, sec_label, egress, protocol, dport
		ValueSize:  24,
		MaxEntries: 16384,
		Flags:      unix.BPF_F_NO_PREALLOC,
	}
)

func policyKey113(i int) []byte {
	k := make([]byte, 8)
	binary.LittleEndian.PutUint32(k[0:], uint32(i))
	binary.BigEndian.PutUint16(k[4:], uint16(i%65535))
	k[6] = 6
	k[7] = byte(i % 2)
	return k
}

func policyKey114(i int) []byte {
	k := make([]byte, 12)
	binary.LittleEndian.PutUint32(k[0:], 64)
	binary.LittleEndian.PutUint32(k[4:], uint32(i))
	k[8] = byte(i % 2)
	k[9] = 6
	binary.BigEndian.PutUint16(k[10:], uint16(i%65535))
	return k
}

func newPolicyMap(t *testing.T, spec *ebpf.MapSpec, key func(int) []byte, n int) *ebpf.Map {
	t.Helper()
	m, err := ebpf.NewMap(spec)
	if err != nil {
		t.Skipf("cannot create BPF maps here: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	value := make([]byte, spec.ValueSize)
	for i := 0; i < n; i++ {
		if err := m.Put(key(i), value); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	return m
}

func TestCountEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *ebpf.MapSpec
		key  func(int) []byte
	}{
		{"cilium-1.13-hash", policyMapSpec113, policyKey113},
		{"cilium-1.14-lpm-trie", policyMapSpec114, policyKey114},
	} {
		for _, n := range []int{0, 1, 4095, 4096, 4097, 10000, 16384} {
			m := newPolicyMap(t, tc.spec, tc.key, n)
			got, err := countEntries(m)
			if err != nil {
				t.Fatalf("%s with %d entries: %v", tc.name, n, err)
			}
			if got != n {
				t.Errorf("%s: countEntries() = %d, want %d", tc.name, got, n)
			}
			if got, _ := countKeys(m); got != n {
				t.Errorf("%s: countKeys() = %d, want %d", tc.name, got, n)
			}
			if tc.spec.Type == ebpf.Hash {
				// countEntries falls back silently, so check the batch path on its own.
				if got, err := countBatch(m); err != nil || got != n {
					t.Errorf("%s: countBatch() = %d, %v, want %d", tc.name, got, err, n)
				}
			}
		}
	}
}

func BenchmarkCountEntries(b *testing.B) {
	for _, tc := range []struct {
		name string
		spec *ebpf.MapSpec
		key  func(int) []byte
	}{
		{"hash", policyMapSpec113, policyKey113},
		{"lpm-trie", policyMapSpec114, policyKey114},
	} {
		b.Run(tc.name, func(b *testing.B) {
			m, err := ebpf.NewMap(tc.spec)
			if err != nil {
				b.Skip(err)
			}
			defer m.Close()
			value := make([]byte, tc.spec.ValueSize)
			for i := 0; i < int(tc.spec.MaxEntries); i++ {
				m.Put(tc.key(i), value)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				countEntries(m)
			}
		})
	}
}

// TestCollector pins policy maps into a private bpffs the way cilium-agent
// does, reads them through the collector with a fake agent API, and checks
// that scrapes are served from the last pass instead of reading the maps.
func TestCollector(t *testing.T) {
	bpffs := t.TempDir()
	if err := unix.Mount("bpf", bpffs, "bpf", 0, ""); err != nil {
		t.Skipf("cannot mount bpffs here: %v", err)
	}
	t.Cleanup(func() { unix.Unmount(bpffs, 0) })
	mapDir := filepath.Join(bpffs, "tc", "globals")
	if err := os.MkdirAll(mapDir, 0o755); err != nil {
		t.Fatal(err)
	}

	pin := func(name string, m *ebpf.Map) {
		if err := m.Pin(filepath.Join(mapDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	web := newPolicyMap(t, policyMapSpec113, policyKey113, 37)
	pin("cilium_policy_00812", web)
	pin("cilium_policy_01504", newPolicyMap(t, policyMapSpec114, policyKey114, 5))
	pin("cilium_policy_03000", newPolicyMap(t, policyMapSpec113, policyKey113, 2)) // not known to the agent
	pin("cilium_call_policy", newPolicyMap(t, &ebpf.MapSpec{Type: ebpf.Hash, KeySize: 4, ValueSize: 4, MaxEntries: 8}, func(i int) []byte { return make([]byte, 4) }, 1))

	sock := serveFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(endpointsJSON))
	})
	c := newPolicyMapCollector("node-a", mapDir, &endpointCache{client: newCiliumClient(sock)}, 5*time.Second, 0)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	start := time.Now()
	c.refresh(context.Background())

	want := `
# HELP cilium_policy_map_entries Number of entries in the endpoint's policy BPF map. Only exported for maps at least --entries-min-usage full.
# TYPE cilium_policy_map_entries gauge
cilium_policy_map_entries{endpoint_id="1504",endpoint_type="host",namespace="",node="node-a",pod=""} 5
cilium_policy_map_entries{endpoint_id="3000",endpoint_type="unknown",namespace="",node="node-a",pod=""} 2
cilium_policy_map_entries{endpoint_id="812",endpoint_type="pod",namespace="default",node="node-a",pod="nginx-7c5ddbdf54-x2x7q"} 37
# HELP cilium_policy_map_max_entries Maximum number of entries a policy BPF map can hold (bpf-policy-map-max). Every endpoint's map has the same size, so this has no labels.
# TYPE cilium_policy_map_max_entries gauge
cilium_policy_map_max_entries 16384
# HELP cilium_custom_node_exporter_cilium_api_up Whether the endpoint list from the cilium-agent API is current (1) or the agent could not be reached (0).
# TYPE cilium_custom_node_exporter_cilium_api_up gauge
cilium_custom_node_exporter_cilium_api_up{node="node-a"} 1
# HELP cilium_custom_node_exporter_errors_total Errors while collecting, by stage (list_maps, read_map, cilium_api).
# TYPE cilium_custom_node_exporter_errors_total counter
cilium_custom_node_exporter_errors_total{node="node-a",stage="cilium_api"} 0
cilium_custom_node_exporter_errors_total{node="node-a",stage="list_maps"} 0
cilium_custom_node_exporter_errors_total{node="node-a",stage="read_map"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"cilium_policy_map_entries", "cilium_policy_map_max_entries",
		"cilium_custom_node_exporter_cilium_api_up", "cilium_custom_node_exporter_errors_total"); err != nil {
		t.Error(err)
	}

	// A new entry shows up only after the next pass, not on scrape.
	if err := web.Put(policyKey113(37), make([]byte, policyMapSpec113.ValueSize)); err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, reg, "cilium_policy_map_entries", "endpoint_id", "812"); got != 37 {
		t.Errorf("entries on scrape before the next pass = %v, want 37 from the last pass", got)
	}
	c.refresh(context.Background())
	if got := gaugeValue(t, reg, "cilium_policy_map_entries", "endpoint_id", "812"); got != 38 {
		t.Errorf("entries after the next pass = %v, want 38", got)
	}

	ts := gaugeValue(t, reg, "cilium_custom_node_exporter_last_collect_timestamp_seconds", "node", "node-a")
	if ts < float64(start.Unix()) || ts > float64(time.Now().Unix()+1) {
		t.Errorf("last collect timestamp = %v, want between %d and now", ts, start.Unix())
	}
}

// gaugeValue gathers reg and returns the value of the name series whose label
// has the given value.
func gaugeValue(t *testing.T, reg prometheus.Gatherer, name, label, value string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("no %s{%s=%q} gathered", name, label, value)
	return 0
}

// A map that disappears between listing and opening (the endpoint was
// deleted) must be recognisable so the collector can skip it quietly.
func TestReadPolicyMapGone(t *testing.T) {
	_, err := readPolicyMap(filepath.Join(t.TempDir(), "cilium_policy_00001"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("readPolicyMap(missing) = %v, want an os.ErrNotExist error", err)
	}
}
