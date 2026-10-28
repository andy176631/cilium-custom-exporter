package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// endpointsJSON is trimmed from a Cilium 1.13 GET /v1/endpoint response:
// a pod, the host endpoint and the health endpoint.
const endpointsJSON = `[
  {"id": 812, "status": {
    "external-identifiers": {"container-id": "3f1c", "k8s-namespace": "default", "k8s-pod-name": "nginx-7c5ddbdf54-x2x7q", "pod-name": "default/nginx-7c5ddbdf54-x2x7q"},
    "identity": {"id": 31337, "labels": ["k8s:app=nginx", "k8s:io.kubernetes.pod.namespace=default"]},
    "labels": {"security-relevant": ["k8s:app=nginx"]},
    "state": "ready"}},
  {"id": 1504, "status": {
    "external-identifiers": {},
    "identity": {"id": 1, "labels": ["reserved:kube-apiserver", "reserved:host"]},
    "state": "ready"}},
  {"id": 2210, "status": {
    "identity": {"id": 4, "labels": ["reserved:health"]},
    "state": "ready"}},
  {"id": 77, "status": {
    "external-identifiers": {"pod-name": "kube-system/coredns-5d78c9869d-8jx2m"},
    "state": "waiting-for-identity"}}
]`

func serveFakeAgent(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "cilium.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestCiliumClientEndpoints(t *testing.T) {
	sock := serveFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/endpoint" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(endpointsJSON))
	})

	got, err := newCiliumClient(sock).endpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint16]endpointInfo{
		812:  {Namespace: "default", Pod: "nginx-7c5ddbdf54-x2x7q", Type: "pod"},
		1504: {Type: "host"},
		2210: {Type: "health"},
		77:   {Namespace: "kube-system", Pod: "coredns-5d78c9869d-8jx2m", Type: "pod"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("endpoints() = %+v, want %+v", got, want)
	}
}

func TestEndpointCache(t *testing.T) {
	var calls, failing atomic.Int32
	sock := serveFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() == 1 {
			http.Error(w, "agent restarting", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(endpointsJSON))
	})
	c := &endpointCache{client: newCiliumClient(sock), ttl: time.Hour}
	ctx := context.Background()

	if _, err := c.lookup(ctx, []uint16{812}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.lookup(ctx, []uint16{812, 1504}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("agent called %d times for known IDs within the TTL, want 1", n)
	}

	// An unknown ID forces a refresh; when that fails the old list is kept.
	failing.Store(1)
	got, err := c.lookup(ctx, []uint16{9999})
	if err == nil {
		t.Error("lookup with the agent down returned no error")
	}
	if got[812].Pod != "nginx-7c5ddbdf54-x2x7q" {
		t.Errorf("lookup with the agent down lost the cached pod, got %+v", got[812])
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("agent called %d times, want 2", n)
	}
}
