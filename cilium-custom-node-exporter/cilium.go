package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// endpointInfo is what the exporter needs to know about a Cilium endpoint.
type endpointInfo struct {
	Namespace string
	Pod       string
	// Type is "pod" for pod endpoints, the reserved identity for the others
	// ("host", "health", "ingress", ...), and "other" when neither is known.
	Type string
}

// apiEndpoint is the subset of the agent's GET /v1/endpoint model
// (api/v1/models.Endpoint) the exporter reads. The fields are the same in
// Cilium 1.13 and 1.14.
type apiEndpoint struct {
	ID     int64 `json:"id"`
	Status struct {
		ExternalIdentifiers struct {
			K8sNamespace string `json:"k8s-namespace"`
			K8sPodName   string `json:"k8s-pod-name"`
			PodName      string `json:"pod-name"` // "<namespace>/<pod>"
		} `json:"external-identifiers"`
		Identity struct {
			Labels []string `json:"labels"`
		} `json:"identity"`
		Labels struct {
			SecurityRelevant []string `json:"security-relevant"`
		} `json:"labels"`
	} `json:"status"`
}

func (ep *apiEndpoint) info() endpointInfo {
	ids := ep.Status.ExternalIdentifiers
	ns, pod := ids.K8sNamespace, ids.K8sPodName
	if pod == "" && ids.PodName != "" {
		if i := strings.IndexByte(ids.PodName, '/'); i >= 0 {
			ns, pod = ids.PodName[:i], ids.PodName[i+1:]
		} else {
			pod = ids.PodName
		}
	}
	if pod != "" {
		return endpointInfo{Namespace: ns, Pod: pod, Type: "pod"}
	}
	return endpointInfo{Type: reservedType(ep.Status.Identity.Labels, ep.Status.Labels.SecurityRelevant)}
}

// reservedType names a non-pod endpoint after its reserved identity label,
// preferring "host" because the host endpoint can also carry
// reserved:kube-apiserver.
func reservedType(labelSets ...[]string) string {
	typ := ""
	for _, labels := range labelSets {
		for _, l := range labels {
			name, ok := strings.CutPrefix(l, "reserved:")
			if !ok {
				continue
			}
			if name == "host" {
				return name
			}
			if typ == "" {
				typ = name
			}
		}
	}
	if typ == "" {
		return "other"
	}
	return typ
}

// ciliumClient talks to the cilium-agent REST API over its unix socket.
type ciliumClient struct {
	http *http.Client
}

func newCiliumClient(socket string) *ciliumClient {
	return &ciliumClient{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

func (c *ciliumClient) endpoints(ctx context.Context) (map[uint16]endpointInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cilium/v1/endpoint", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET /v1/endpoint: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	var eps []apiEndpoint
	if err := json.NewDecoder(resp.Body).Decode(&eps); err != nil {
		return nil, fmt.Errorf("decoding /v1/endpoint: %w", err)
	}
	byID := make(map[uint16]endpointInfo, len(eps))
	for i := range eps {
		byID[uint16(eps[i].ID)] = eps[i].info()
	}
	return byID, nil
}

// endpointCache keeps the last endpoint list fetched from cilium-agent so a
// scrape does not have to ask the agent every time, and so pod labels
// survive an agent restart.
type endpointCache struct {
	client *ciliumClient
	ttl    time.Duration

	mu        sync.Mutex
	byID      map[uint16]endpointInfo
	fetchedAt time.Time
}

// lookup returns endpoint metadata covering ids. It asks the agent again
// when the cached list is older than ttl or misses one of ids. If the agent
// cannot be reached, it returns the last good list along with the error.
func (c *endpointCache) lookup(ctx context.Context, ids []uint16) (map[uint16]endpointInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.ttl && c.hasAll(ids) {
		return c.byID, nil
	}
	byID, err := c.client.endpoints(ctx)
	if err != nil {
		return c.byID, err
	}
	c.byID, c.fetchedAt = byID, time.Now()
	return byID, nil
}

func (c *endpointCache) hasAll(ids []uint16) bool {
	for _, id := range ids {
		if _, ok := c.byID[id]; !ok {
			return false
		}
	}
	return true
}
