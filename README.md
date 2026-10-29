# cilium-custom-exporter

Cilium 的自訂 Prometheus exporter（node exporter 支援 1.13 / 1.14 / 1.16，cluster exporter 支援 1.13 / 1.14），分成兩個獨立的程式，各自有自己的 Go module、Dockerfile 和部署 YAML：

| 目錄 | 部署方式 | 資料來源 | 匯出內容 |
|---|---|---|---|
| [`cilium-custom-node-exporter/`](cilium-custom-node-exporter/README.md) | DaemonSet（每個節點一個） | 節點上 pinned 的 BPF map、cilium-agent API socket | 每個 endpoint policy BPF map 的 entry 數量（帶 `pod`、`namespace`、`node`） |
| [`cilium-custom-cluster-exporter/`](cilium-custom-cluster-exporter/README.md) | Deployment（整個 cluster 一個） | Kubernetes API server 的 `CiliumIdentity` | identity 總數、每個 namespace 的數量、每個 security label key 的不同值數量 |

`cilium-custom-node-exporter` 就是原本的 `cilium-custom-exporter`，改名後移到子目錄。它自己的 metric（`cilium_custom_exporter_*`）也一起改成 `cilium_custom_node_exporter_*`，`cilium_policy_map_*` 不變。

兩個 exporter 的資源都放在 `cilium` namespace（要先存在，YAML 不會建立或刪除它），而且都帶 `app.kubernetes.io/part-of: cilium-custom-exporter` label。

## 快速部署

```bash
# 先把兩個 YAML 裡的 image 換成你建置並推上去的 image
kubectl apply -f cilium-custom-node-exporter/deploy/daemonset.yaml
kubectl apply -f cilium-custom-cluster-exporter/deploy/deployment.yaml

# 有 Prometheus Operator 時
kubectl apply -f cilium-custom-node-exporter/deploy/servicemonitor.yaml \
              -f cilium-custom-node-exporter/deploy/prometheusrule.yaml \
              -f cilium-custom-cluster-exporter/deploy/servicemonitor.yaml
# 還沒有 Prometheus 的話，再加這個（會同時抓兩個 exporter）
kubectl apply -f deploy/prometheus.yaml
```

建置與測試在各自的目錄執行 `make image` / `make test`，細節見各自的 README。
