package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"

	"github.com/cilium/ebpf"
)

// policyMapRe matches the per-endpoint policy maps cilium-agent pins as
// cilium_policy_%05d (pkg/bpf.LocalMapName in 1.13 and 1.14). It leaves out
// the tail-call maps cilium_call_policy and cilium_egresscall_policy, and
// half-written pins with a suffix such as ":pending".
var policyMapRe = regexp.MustCompile(`^cilium_policy_(\d+)$`)

type policyMapFile struct {
	EndpointID uint16
	Path       string
}

// listPolicyMaps returns the endpoint policy maps pinned in dir.
func listPolicyMaps(dir string) ([]policyMapFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var maps []policyMapFile
	seen := make(map[uint16]bool)
	for _, e := range entries {
		m := policyMapRe.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		id, err := strconv.ParseUint(m[1], 10, 16)
		if err != nil || seen[uint16(id)] {
			continue
		}
		seen[uint16(id)] = true
		maps = append(maps, policyMapFile{EndpointID: uint16(id), Path: filepath.Join(dir, e.Name())})
	}
	return maps, nil
}

type mapStats struct {
	Entries    int
	MaxEntries uint32
}

// readPolicyMap opens a pinned policy map read-only and counts its entries.
func readPolicyMap(path string) (mapStats, error) {
	m, err := ebpf.LoadPinnedMap(path, &ebpf.LoadPinOptions{ReadOnly: true})
	if err != nil {
		return mapStats{}, err
	}
	defer m.Close()

	n, err := countEntries(m)
	if err != nil {
		return mapStats{}, err
	}
	return mapStats{Entries: n, MaxEntries: m.MaxEntries()}, nil
}

// countEntries counts the keys in m. Hash maps (the policy map type in
// Cilium 1.13) are read with BPF_MAP_LOOKUP_BATCH. Other maps, such as the
// LPM trie Cilium 1.14 uses, have no batch support, so their keys are walked
// one at a time. The walk is also the fallback if a batch read fails.
func countEntries(m *ebpf.Map) (int, error) {
	if m.Type() == ebpf.Hash || m.Type() == ebpf.LRUHash {
		if n, err := countBatch(m); err == nil {
			return n, nil
		}
	}
	return countKeys(m)
}

const batchSize = 4096

func countBatch(m *ebpf.Map) (int, error) {
	keys := byteArraySlice(m.KeySize(), batchSize)
	values := byteArraySlice(m.ValueSize(), batchSize)
	var cursor ebpf.MapBatchCursor
	total := 0
	for {
		n, err := m.BatchLookup(&cursor, keys, values, nil)
		total += n
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return total, nil
		}
		if err != nil {
			return 0, err
		}
		if total > 2*int(m.MaxEntries()) {
			return 0, errors.New("batch lookup did not reach the end of the map")
		}
	}
}

// byteArraySlice returns a [][size]byte of length n. BatchLookup wants one
// slice element per key or value, and the sizes are only known at runtime.
func byteArraySlice(size uint32, n int) any {
	t := reflect.SliceOf(reflect.ArrayOf(int(size), reflect.TypeOf(byte(0))))
	return reflect.MakeSlice(t, n, n).Interface()
}

// countKeys walks m with BPF_MAP_GET_NEXT_KEY. When the key the walk stands
// on is deleted concurrently the kernel starts over from the first key, so
// keys are de-duplicated and the number of steps is bounded.
func countKeys(m *ebpf.Map) (int, error) {
	key := make([]byte, m.KeySize())
	next := make([]byte, m.KeySize())
	seen := make(map[string]struct{})
	var prev any // nil asks the kernel for the first key
	limit := 4 * (int(m.MaxEntries()) + 1)
	for i := 0; i < limit; i++ {
		err := m.NextKey(prev, next)
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return len(seen), nil
		}
		if err != nil {
			return 0, err
		}
		seen[string(next)] = struct{}{}
		key, next = next, key
		prev = key
	}
	return 0, fmt.Errorf("map changed too much to count after %d steps", limit)
}
