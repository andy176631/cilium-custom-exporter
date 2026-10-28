package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/cache"
)

// newIdentity builds a CiliumIdentity the way cilium-operator / cilium-agent
// create them in 1.13 and 1.14.
func newIdentity(id int, labels map[string]string) *unstructured.Unstructured {
	sl := map[string]any{}
	meta := map[string]any{}
	for k, v := range labels {
		sl[k] = v
		meta[strings.TrimPrefix(k, "k8s:")] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumIdentity",
		"metadata": map[string]any{
			"name":   fmt.Sprint(id),
			"labels": meta,
			"managedFields": []any{
				map[string]any{"manager": "cilium-agent", "operation": "Update"},
			},
		},
		"security-labels": sl,
	}}
}

func podLabels(ns string, kv ...string) map[string]string {
	l := map[string]string{
		namespaceLabel:                            ns,
		"k8s:io.cilium.k8s.policy.cluster":        "default",
		"k8s:io.cilium.k8s.policy.serviceaccount": "default",
	}
	for i := 0; i < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return l
}

func TestAggregateIdentities(t *testing.T) {
	ids := []identity{
		{name: "1001", namespace: "payments", labels: podLabels("payments", "k8s:app", "api", "k8s:rollouts-pod-template-hash", "a1")},
		{name: "1002", namespace: "payments", labels: podLabels("payments", "k8s:app", "api", "k8s:rollouts-pod-template-hash", "a2")},
		{name: "1003", namespace: "payments", labels: podLabels("payments", "k8s:app", "worker", "k8s:rollouts-pod-template-hash", "b1")},
		{name: "1004", namespace: "default", labels: podLabels("default", "k8s:app", "api")},
		{name: "1005", labels: map[string]string{"k8s:some-cluster-wide": "x"}},
	}
	a := aggregateIdentities(ids, aggregateOptions{excludeKeys: map[string]bool{"k8s:rollouts-pod-template-hash": true}})

	if a.total != 5 {
		t.Errorf("total = %d, want 5", a.total)
	}
	if want := map[string]int{"payments": 3, "default": 1, "": 1}; fmt.Sprint(a.perNS) != fmt.Sprint(want) {
		t.Errorf("perNS = %v, want %v", a.perNS, want)
	}
	for k, want := range map[nsKey]int{
		{"payments", "k8s:rollouts-pod-template-hash"}: 3,
		{"payments", "k8s:app"}:                        2,
		{"default", "k8s:app"}:                         1,
		{"", "k8s:some-cluster-wide"}:                  1,
	} {
		if got := a.distinct[k]; got != want {
			t.Errorf("distinct%v = %d, want %d", k, got, want)
		}
	}
	if _, ok := a.distinct[nsKey{"payments", namespaceLabel}]; ok {
		t.Error("the namespace label itself should not be reported")
	}
	if got := a.valueCounts[labelValue{"payments", "k8s:app", "api"}]; got != 2 {
		t.Errorf("payments app=api = %d, want 2", got)
	}
	for lv := range a.valueCounts {
		if lv.key == "k8s:rollouts-pod-template-hash" {
			t.Errorf("excluded key exported: %v", lv)
		}
	}
	// One excluded key in one namespace.
	if a.suppressed != 1 {
		t.Errorf("suppressed = %d, want 1", a.suppressed)
	}

	// The per-key cap suppresses k8s:app in payments (2 values > 1) but not
	// in default (1 value).
	a = aggregateIdentities(ids, aggregateOptions{maxValuesPerKey: 1})
	if _, ok := a.valueCounts[labelValue{"payments", "k8s:app", "api"}]; ok {
		t.Error("k8s:app in payments should be over the cap")
	}
	if got := a.valueCounts[labelValue{"default", "k8s:app", "api"}]; got != 1 {
		t.Errorf("default app=api = %d, want 1", got)
	}
}

func TestStripIdentity(t *testing.T) {
	in := newIdentity(1001, podLabels("payments", "k8s:app", "api"))
	out, err := stripIdentity(in)
	if err != nil {
		t.Fatal(err)
	}
	u := out.(*unstructured.Unstructured)
	if u.GetName() != "1001" || len(u.GetManagedFields()) != 0 || u.GetLabels() != nil {
		t.Errorf("not stripped: %v", u.Object)
	}
	id, _ := identityFromObject(u)
	if id.namespace != "payments" || id.labels["k8s:app"] != "api" {
		t.Errorf("identity = %+v", id)
	}

	// Without the namespace security label, fall back to metadata.labels.
	in = newIdentity(1002, map[string]string{"k8s:app": "api"})
	in.SetLabels(map[string]string{"io.kubernetes.pod.namespace": "default"})
	out, _ = stripIdentity(in)
	if id, _ := identityFromObject(out); id.namespace != "default" {
		t.Errorf("namespace = %q, want default", id.namespace)
	}

	// Tombstones pass through untouched.
	tomb := cache.DeletedFinalStateUnknown{Key: "1003"}
	if out, _ := stripIdentity(tomb); out != tomb {
		t.Errorf("tombstone changed: %v", out)
	}
}

// TestInformerToMetrics runs the informer the way main does against a fake API
// server and checks the scraped metrics follow adds and deletes.
func TestInformerToMetrics(t *testing.T) {
	scheme := runtime.NewScheme()
	var objs []runtime.Object
	for i := 0; i < 37; i++ {
		objs = append(objs, newIdentity(2000+i, podLabels("payments", "k8s:app", "api", "k8s:rollouts-pod-template-hash", fmt.Sprintf("h%02d", i))))
	}
	objs = append(objs, newIdentity(3000, podLabels("default", "k8s:app", "web")))
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{identityGVR: "CiliumIdentityList"}, objs...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory := dynamicinformer.NewDynamicSharedInformerFactory(client, 0)
	informer := factory.ForResource(identityGVR).Informer()
	if err := informer.SetTransform(stripIdentity); err != nil {
		t.Fatal(err)
	}
	c := newIdentityCollector(informer.GetStore(), informer.HasSynced,
		aggregateOptions{excludeKeys: map[string]bool{"k8s:rollouts-pod-template-hash": true}, maxValuesPerKey: 50}, false)

	// Before the cache syncs only cache_synced is exported.
	c.refresh()
	if n := testutil.CollectAndCount(c); n != 1 {
		t.Fatalf("%d metrics before sync, want 1", n)
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		t.Fatal("cache did not sync")
	}
	c.refresh()

	want := `
# HELP cilium_identity_total Number of CiliumIdentity objects in the cluster.
# TYPE cilium_identity_total gauge
cilium_identity_total 38
# HELP cilium_identity_count Number of CiliumIdentity objects per namespace (from the k8s:io.kubernetes.pod.namespace security label; empty for identities without one).
# TYPE cilium_identity_count gauge
cilium_identity_count{namespace="default"} 1
cilium_identity_count{namespace="payments"} 37
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "cilium_identity_total", "cilium_identity_count"); err != nil {
		t.Error(err)
	}
	if v := metricValue(t, c, "cilium_identity_label_distinct_values", `key="k8s:rollouts-pod-template-hash",namespace="payments"`); v != 37 {
		t.Errorf("distinct rollouts-pod-template-hash = %v, want 37", v)
	}
	if v := metricValue(t, c, "cilium_identity_security_label", `key="k8s:app",namespace="payments",value="api"`); v != 37 {
		t.Errorf("payments app=api = %v, want 37", v)
	}
	if n := testutil.CollectAndCount(c, "cilium_identity_security_label"); n != 6 {
		// payments: app, cluster, serviceaccount; default: the same three.
		t.Errorf("%d cilium_identity_security_label series, want 6", n)
	}

	// Deletes reach the metrics on the next refresh.
	res := client.Resource(identityGVR)
	for i := 0; i < 10; i++ {
		if err := res.Delete(ctx, fmt.Sprint(2000+i), metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return len(informer.GetStore().ListKeys()) == 28 })
	c.refresh()
	if v := metricValue(t, c, "cilium_identity_total", ""); v != 28 {
		t.Errorf("total after delete = %v, want 28", v)
	}
	if v := metricValue(t, c, "cilium_identity_label_distinct_values", `key="k8s:rollouts-pod-template-hash",namespace="payments"`); v != 27 {
		t.Errorf("distinct after delete = %v, want 27", v)
	}
}

func TestIdentityInfo(t *testing.T) {
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	obj, _ := stripIdentity(newIdentity(1001, podLabels("payments", "k8s:app", "api")))
	store.Add(obj)
	c := newIdentityCollector(store, func() bool { return true }, aggregateOptions{}, true)
	c.refresh()
	want := `
# HELP cilium_identity_info One series per CiliumIdentity with all its security labels. Only exported with --identity-info.
# TYPE cilium_identity_info gauge
cilium_identity_info{identity="1001",labels="k8s:app=api,k8s:io.cilium.k8s.policy.cluster=default,k8s:io.cilium.k8s.policy.serviceaccount=default,k8s:io.kubernetes.pod.namespace=payments",namespace="payments"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "cilium_identity_info"); err != nil {
		t.Error(err)
	}
}

func metricValue(t *testing.T, c *identityCollector, name, labels string) float64 {
	t.Helper()
	out, err := testutil.CollectAndFormat(c, expfmt.TypeTextPlain, name)
	if err != nil {
		t.Fatal(err)
	}
	prefix := name + " "
	if labels != "" {
		prefix = name + "{" + labels + "} "
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			var f float64
			fmt.Sscan(v, &f)
			return f
		}
	}
	t.Fatalf("no %s in\n%s", prefix, out)
	return 0
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
