# cilium-policy-exporter

以 DaemonSet 跑在每個節點上的 Prometheus exporter，匯出該節點上**每一個 Cilium endpoint policy BPF map 的 entry 數量**，每筆 metric 都帶 `pod`、`namespace`、`node` label。

支援環境：Cilium 1.13 / 1.14、amd64、Linux kernel 6.2（Ubuntu 22.04）。

## Metrics

| Metric | 類型 | 說明 |
|---|---|---|
| `cilium_policy_map_entries` | gauge | 該 endpoint policy map 目前的 entry 數 |
| `cilium_policy_map_max_entries` | gauge | 該 map 的上限（Cilium 預設 16384，對應 `bpf-policy-map-max`） |
| `cilium_policy_exporter_cilium_api_up` | gauge | 最近一次向 cilium-agent 取 endpoint 清單是否成功（1/0） |
| `cilium_policy_exporter_errors_total{stage}` | counter | 收集錯誤次數，`stage` = `list_maps` / `read_map` / `cilium_api` |
| `cilium_policy_exporter_collect_duration_seconds` | gauge | 這次 scrape 讀完所有 map 花的時間 |

map 相關 metric 的 label：

| Label | 內容 |
|---|---|
| `node` | 節點名稱（Downward API 的 `spec.nodeName`） |
| `namespace` | Pod 的 namespace |
| `pod` | Pod 名稱 |
| `endpoint_id` | Cilium endpoint ID（map 檔名 `cilium_policy_XXXXX` 的數字） |
| `endpoint_type` | `pod`；非 Pod 的 endpoint 用其 reserved identity，例如 `host`、`health`、`ingress`；cilium-agent 查不到的 map 是 `unknown` |

範例輸出：

```
cilium_policy_map_entries{endpoint_id="812",endpoint_type="pod",namespace="default",node="worker-1",pod="nginx-7c5ddbdf54-x2x7q"} 37
cilium_policy_map_entries{endpoint_id="1504",endpoint_type="host",namespace="",node="worker-1",pod=""} 9
cilium_policy_map_max_entries{endpoint_id="812",endpoint_type="pod",namespace="default",node="worker-1",pod="nginx-7c5ddbdf54-x2x7q"} 16384
```

`host`、`health` 這類 endpoint 不屬於任何 Pod，所以 `pod`、`namespace` 是空字串，但 map 一樣會匯出（host firewall 的規則就在 host endpoint 的 map 裡）。

## 運作方式

1. **找 map**：讀 `/sys/fs/bpf/tc/globals/` 下符合 `cilium_policy_<endpoint ID>` 的 pinned map（Cilium 1.13 / 1.14 都是用 `cilium_policy_%05d` 命名）。`cilium_call_policy`、`cilium_egresscall_policy` 是 tail call 用的 map，不算在內。
2. **數 entry**：以唯讀方式開啟 map。
   - Cilium 1.13 的 policy map 是 **hash map**（key 8 bytes），用 `BPF_MAP_LOOKUP_BATCH` 一次讀 4096 筆，很快。
   - Cilium 1.14 改成 **LPM trie**（key 12 bytes，多了 prefixlen），kernel 不支援 batch，所以逐一走 key（`BPF_MAP_GET_NEXT_KEY`）。在測試環境中，16384 筆的滿 map 約 16 ms（hash map 用 batch 約 2 ms）。
   - map 在讀的同時被 cilium-agent 修改時，kernel 可能從頭重走，所以 key 會去重，步數也有上限，不會卡死或重複計算。
3. **對應 Pod**：透過 cilium-agent 的 unix socket（`/var/run/cilium/cilium.sock`）呼叫 `GET /v1/endpoint`，用 `status.external-identifiers` 裡的 `k8s-namespace` / `k8s-pod-name` 把 endpoint ID 對應到 Pod。這個 API 欄位在 1.13 / 1.14 相同，不需要 Kubernetes API 權限，也不需要 RBAC。
   - endpoint 清單快取 30 秒；出現新的 endpoint ID 會立刻重抓。
   - cilium-agent 重啟或連不上時，沿用最後一次成功的清單，map 數量照樣匯出，同時 `cilium_policy_exporter_cilium_api_up` 變成 0。

## 建置 image

```bash
make image IMAGE=<你的 registry>/cilium-policy-exporter:0.1.0
make push  IMAGE=<你的 registry>/cilium-policy-exporter:0.1.0
```

或直接 `docker build -t <image> .`。產出的是靜態連結的 amd64 binary，base image 是 `gcr.io/distroless/static-debian12`。

## 部署

1. 把 `deploy/daemonset.yaml` 裡的 `image: registry.example.com/cilium-policy-exporter:0.1.0` 換成你推上去的 image。
2. 套用：

```bash
kubectl apply -f deploy/daemonset.yaml
kubectl -n cilium-policy-exporter rollout status ds/cilium-policy-exporter
```

YAML 的重點：

- `hostNetwork: true`，listen 在節點的 `9969` port。這樣就算該節點的 Cilium datapath 出問題，exporter 一樣能被 scrape。若 9969 已被占用，改 `--listen-address`、`containerPort` 和 `prometheus.io/port` 三處。
- `privileged: true`，讀 pinned BPF map 需要 `CAP_BPF` / `CAP_SYS_ADMIN`。
- 唯讀掛載 host 的 `/sys/fs/bpf`（`mountPropagation: HostToContainer`，所以 cilium-agent 比 exporter 晚掛 bpffs 也看得到）和 `/var/run/cilium`（API socket）。
- `tolerations: [{operator: Exists}]`，所有節點都會跑，包含 control-plane。
- 不使用 ServiceAccount token，不需要任何 RBAC。
- Namespace 加了 `pod-security.kubernetes.io/enforce: privileged`，叢集有開 Pod Security Admission 也能建立。

確認輸出：

```bash
kubectl -n cilium-policy-exporter get pods -o wide
curl -s http://<node IP>:9969/metrics | grep ^cilium_policy
```

## 接 Prometheus

- **用 annotation 自動發現 Pod**（例如 prometheus community chart 的 `kubernetes-pods` job）：DaemonSet 已經帶 `prometheus.io/scrape: "true"`、`prometheus.io/port: "9969"`，不用額外設定。
- **用 Prometheus Operator**：另外套用 `deploy/servicemonitor.yaml`。

不管哪一種，scrape 設定都要開 `honor_labels: true`（ServiceMonitor 裡是 `honorLabels: true`，範例檔已經設好）。不開的話，Prometheus 會用 exporter 自己那顆 Pod 的 `namespace` / `pod` 蓋掉，原本的值會被改名成 `exported_namespace` / `exported_pod`。

## PromQL 範例

```promql
# 每個 Pod 的 policy map 使用率
cilium_policy_map_entries / cilium_policy_map_max_entries

# entry 數最多的 10 個 Pod
topk(10, cilium_policy_map_entries{endpoint_type="pod"})

# 某個節點上所有 policy map 的 entry 總和
sum by (node) (cilium_policy_map_entries)
```

告警範例（map 滿了以後，該 endpoint 的 policy 更新會寫不進去，新允許的流量可能被擋）：

```yaml
- alert: CiliumPolicyMapAlmostFull
  expr: cilium_policy_map_entries / cilium_policy_map_max_entries > 0.9
  for: 10m
  labels:
    severity: warning
  annotations:
    summary: "{{ $labels.namespace }}/{{ $labels.pod }} 的 Cilium policy map 已用 {{ $value | humanizePercentage }}"
```

## 參數

| 參數 | 預設值 | 說明 |
|---|---|---|
| `--listen-address` | `:9969` | `/metrics`、`/healthz` 的 listen 位址 |
| `--bpf-map-dir` | `/sys/fs/bpf/tc/globals` | cilium-agent pin map 的目錄 |
| `--cilium-socket` | `/var/run/cilium/cilium.sock` | cilium-agent API socket |
| `--node-name` | `$NODE_NAME`，再不行用 hostname | `node` label 的值 |
| `--endpoint-cache-ttl` | `30s` | endpoint 清單快取時間 |
| `--cilium-api-timeout` | `5s` | 呼叫 cilium-agent API 的 timeout |

## 疑難排解

| 現象 | 可能原因 |
|---|---|
| 沒有任何 `cilium_policy_map_entries` | 該節點的 cilium-agent 還沒起來，或 bpffs 不是掛在 `/sys/fs/bpf`（看 `cilium_policy_exporter_errors_total{stage="list_maps"}` 和 log） |
| `endpoint_type="unknown"`、`pod` 是空的 | cilium-agent API 查不到這個 endpoint，通常是 agent 從沒連上過，或 agent 異常結束後殘留的 map。檢查 `cilium_policy_exporter_cilium_api_up` |
| `cilium_policy_exporter_cilium_api_up` = 0 | `/var/run/cilium/cilium.sock` 連不上，看 exporter log 的錯誤訊息 |
| `errors_total{stage="read_map"}` 持續增加 | 權限不足（確認 `privileged: true`），或 map 格式不符，log 裡有完整錯誤 |

## 開發與測試

```bash
make test   # 用 root 執行才會跑 BPF 相關測試，否則會 skip
```

測試內容：

- 依 Cilium 1.13（hash）和 1.14（LPM trie）原始碼裡 `struct policy_key` / `policy_entry` 的大小建立真的 BPF map，在 0、1、4095、4096、4097、10000、16384 筆時驗證計數正確。
- 掛一個私有 bpffs，照 cilium-agent 的命名 pin map，配一個假的 cilium-agent API，從 `/metrics` 驗證 label 和數值。
- cilium-agent API 的 JSON 解析、快取、agent 斷線時沿用舊資料。
