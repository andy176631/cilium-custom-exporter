package main

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var identityGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumidentities"}

const (
	// securityLabelsField is the top-level field of a CiliumIdentity holding
	// the labels the identity was allocated for (not under spec).
	securityLabelsField = "security-labels"
	// namespaceLabel is the security label carrying the pod's namespace. The
	// same key without the "k8s:" source is also set in metadata.labels.
	namespaceLabel = "k8s:io.kubernetes.pod.namespace"
)

// stripIdentity is the informer's transform: it keeps only what the exporter
// reads (name and security labels), so a cluster with many identities does not
// hold their managedFields and other metadata in memory.
func stripIdentity(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		// cache.DeletedFinalStateUnknown and friends are passed through.
		return obj, nil
	}
	labels, _, err := unstructured.NestedStringMap(u.Object, securityLabelsField)
	if err != nil {
		return nil, fmt.Errorf("CiliumIdentity %s: %w", u.GetName(), err)
	}
	if _, ok := labels[namespaceLabel]; !ok {
		if ns, ok := u.GetLabels()["io.kubernetes.pod.namespace"]; ok {
			if labels == nil {
				labels = map[string]string{}
			}
			labels[namespaceLabel] = ns
		}
	}
	out := &unstructured.Unstructured{Object: map[string]any{}}
	out.SetAPIVersion(u.GetAPIVersion())
	out.SetKind(u.GetKind())
	out.SetName(u.GetName())
	out.SetResourceVersion(u.GetResourceVersion())
	if labels != nil {
		sl := make(map[string]any, len(labels))
		for k, v := range labels {
			sl[k] = v
		}
		out.Object[securityLabelsField] = sl
	}
	return out, nil
}

// identity is a CiliumIdentity as the exporter sees it.
type identity struct {
	name      string // the numeric identity
	namespace string // "" for identities that do not belong to a namespace
	labels    map[string]string
}

func identityFromObject(obj any) (identity, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return identity{}, false
	}
	labels, _, _ := unstructured.NestedStringMap(u.Object, securityLabelsField)
	return identity{name: u.GetName(), namespace: labels[namespaceLabel], labels: labels}, true
}

type nsKey struct{ namespace, key string }

// aggregate is everything the exporter reports, computed from all identities.
type aggregate struct {
	total    int
	perNS    map[string]int
	distinct map[nsKey]int
}

func aggregateIdentities(ids []identity) aggregate {
	a := aggregate{
		total:    len(ids),
		perNS:    map[string]int{},
		distinct: map[nsKey]int{},
	}
	values := map[nsKey]map[string]struct{}{}
	for _, id := range ids {
		a.perNS[id.namespace]++
		for k, v := range id.labels {
			if k == namespaceLabel {
				continue // already the namespace label of every series
			}
			nk := nsKey{id.namespace, k}
			if values[nk] == nil {
				values[nk] = map[string]struct{}{}
			}
			values[nk][v] = struct{}{}
		}
	}
	for nk, vals := range values {
		a.distinct[nk] = len(vals)
	}
	return a
}
