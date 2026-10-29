# cilium-custom-node-exporter

以 DaemonSet 跑在每個節點上的 Prometheus exporter，匯出該節點上**每一個 Cilium endpoint policy BPF map 的 entry 數量**，每筆都帶 `node`、`namespace`、`pod` label。用來在 policy map 滿之前發現問題（map 滿了以後，新的 policy 規則會寫不進去，該允許的流量可能被擋）。

- 支援版本：Cilium 1.13、1.14、1.16（1.16.7 已實測，程式不需修改）
- 環境：amd64、Linux kernel 6.2（Ubuntu 22.04）
- 部署：`deploy/daemonset.yaml`，namespace `cilium`
- 以下指令都在 `cilium-custom-node-exporter/` 目錄下執行；整個 repo 的介紹見[上層 README](../README.md)

## 運作機制

exporter 在背景每 `--collect-interval`（預設 30 秒）做一輪下面的步驟，結果存在記憶體。Prometheus scrape 時只回傳最近一輪的結果，不會觸發任何 bpf syscall，也不會呼叫 cilium-agent API，所以 scrape 頻率和 Prometheus 數量都不影響節點負載。代價是數字最多比實際晚一個 collect interval。

```
 /sys/fs/bpf/tc/globals/cilium_policy_XXXXX ──(1) 列出 map ──┐
                                                            ├─(2) 唯讀開啟、數 entry ─┐
 /var/run/cilium/cilium.sock  GET /v1/endpoint ──(3) endpoint ID → Pod ──────────────┤
                                                                                    ▼
                                                              (4) 記憶體快照 ──► /metrics
```

1. **找出 policy map**：讀 `--bpf-map-dir`（預設 `/sys/fs/bpf/tc/globals`）底下名稱符合 `cilium_policy_<endpoint ID>` 的 pinned map，每個 endpoint 一個。tail call 用的 `cilium_call_policy`、`cilium_egresscall_policy`，以及還沒寫完的 `:pending` 檔不算在內。
2. **數 entry**：以唯讀方式開啟 map 計算 entry 數，同時讀出 map 的上限（`max_entries`）。
   - 支援 batch 的 map（Cilium 1.13 的 hash map）用 `BPF_MAP_LOOKUP_BATCH` 一次讀 4096 筆。
   - 不支援 batch 的 map（Cilium 1.14 起的 LPM trie）用 `BPF_MAP_GET_NEXT_KEY` 逐一走 key。16384 筆的滿 map 約 16 ms。
   - cilium-agent 在讀取途中修改 map 時，kernel 可能從頭重走，所以 key 會去重、步數有上限，不會卡住或重複計算。
   - endpoint 在讀取途中被刪掉的 map 直接略過。
3. **對應到 Pod**：透過 cilium-agent 的 unix socket（`--cilium-socket`）呼叫 `GET /v1/endpoint`，用 `status.external-identifiers` 的 `k8s-namespace`、`k8s-pod-name` 把 endpoint ID 對應到 Pod。不需要 Kubernetes API 權限。
   - endpoint 清單快取 `--endpoint-cache-ttl`（預設 30 秒）；出現清單裡沒有的 endpoint ID 時會立刻重抓。
   - 不是 Pod 的 endpoint（host、health、ingress 等）用它的 reserved identity 當 `endpoint_type`，`pod`、`namespace` 是空字串。
   - cilium-agent 重啟或連不上時，沿用最後一次成功的清單，map 數量照樣匯出，同時 `cilium_custom_node_exporter_cilium_api_up` 變成 0。查不到的 endpoint 的 `endpoint_type` 是 `unknown`。
4. **更新快照**：整輪結果原子地替換記憶體裡的快照。第一輪在開始服務 HTTP 之前就先跑完，所以第一次 scrape 就有資料。

效能參考：一個節點 100 個 endpoint、每個 map 1000 筆時，一輪約 28 ms（1.13）、117 ms（1.14）。

## Metrics

| Metric | 類型 | Label | 說明 |
|---|---|---|---|
| `cilium_policy_map_entries` | gauge | `node`, `namespace`, `pod`, `endpoint_id`, `endpoint_type` | 該 endpoint policy map 目前的 entry 數 |
| `cilium_policy_map_max_entries` | gauge | （無） | policy map 的上限（Cilium 的 `bpf-policy-map-max`，預設 16384）。每個 endpoint 的 map 大小相同，所以只有一筆；讀不到任何 map 時不匯出 |
| `cilium_custom_node_exporter_cilium_api_up` | gauge | `node` | 最近一輪向 cilium-agent 取 endpoint 清單是否成功（1 / 0） |
| `cilium_custom_node_exporter_errors_total` | counter | `node`, `stage` | 收集錯誤次數。`stage`：`list_maps`（列 map 目錄失敗）、`read_map`（讀某個 map 失敗）、`cilium_api`（呼叫 cilium-agent 失敗） |
| `cilium_custom_node_exporter_collect_duration_seconds` | gauge | `node` | 最近一輪花的時間 |
| `cilium_custom_node_exporter_last_collect_timestamp_seconds` | gauge | `node` | 最近一輪完成的時間（Unix 秒），用來確認資料有沒有過期 |

`cilium_policy_map_entries` 的 label：

| Label | 內容 |
|---|---|
| `node` | 節點名稱（`--node-name`，DaemonSet 用 Downward API 帶入 `spec.nodeName`） |
| `namespace` | Pod 的 namespace；非 Pod endpoint 為空字串 |
| `pod` | Pod 名稱；非 Pod endpoint 為空字串 |
| `endpoint_id` | Cilium endpoint ID（map 檔名 `cilium_policy_XXXXX` 的數字） |
| `endpoint_type` | Pod 為 `pod`；其他用 reserved identity，例如 `host`、`health`、`ingress`；cilium-agent 查不到的為 `unknown` |

範例輸出：

```
cilium_policy_map_entries{endpoint_id="812",endpoint_type="pod",namespace="default",node="worker-1",pod="nginx-7c5ddbdf54-x2x7q"} 37
cilium_policy_map_entries{endpoint_id="1504",endpoint_type="host",namespace="",node="worker-1",pod=""} 9
cilium_policy_map_max_entries 16384
cilium_custom_node_exporter_cilium_api_up{node="worker-1"} 1
```

在 Prometheus 裡，`cilium_policy_map_max_entries` 會因為每個節點各是一個 scrape target 而存成每節點一筆。`deploy/prometheusrule.yaml` 提供 recording rule `cluster:cilium_policy_map_max_entries:max` 把它收成整個 cluster 一筆。

PromQL 範例：

```promql
# 每個 Pod 的 policy map 使用率
cilium_policy_map_entries / on() group_left cluster:cilium_policy_map_max_entries:max

# 使用率超過 90%
cilium_policy_map_entries / on() group_left cluster:cilium_policy_map_max_entries:max > 0.9

# entry 數最多的 10 個 Pod
topk(10, cilium_policy_map_entries{endpoint_type="pod"})
```

沒有套用 recording rule 的話，把 `cluster:cilium_policy_map_max_entries:max` 換成 `max(cilium_policy_map_max_entries)`。

## 參數

| 參數 | 型別 | 預設值 | 說明 |
|---|---|---|---|
| `--listen-address` | string | `:9969` | `/metrics`、`/healthz` 的 listen 位址 |
| `--collect-interval` | duration | `30s` | 背景讀取所有 policy map 的間隔，必須大於 0。scrape 只回傳最近一輪的結果，建議不要比 Prometheus 的 scrape interval 長 |
| `--bpf-map-dir` | string | `/sys/fs/bpf/tc/globals` | cilium-agent pin BPF map 的目錄 |
| `--cilium-socket` | string | `/var/run/cilium/cilium.sock` | cilium-agent API 的 unix socket |
| `--node-name` | string | `$NODE_NAME`，沒有則用 hostname | `node` label 的值 |
| `--endpoint-cache-ttl` | duration | `30s` | endpoint 清單的快取時間；出現新的 endpoint ID 時不論 TTL 都會重抓 |
| `--cilium-api-timeout` | duration | `5s` | 每次呼叫 cilium-agent API 的 timeout |

duration 的格式是 Go 的 `time.ParseDuration`，例如 `15s`、`1m`。

HTTP endpoint：`/metrics`（Prometheus 格式）、`/healthz`（固定回 `ok`）。

## 建置與部署

```bash
make image IMAGE=<registry>/cilium-custom-node-exporter:0.1.0
make push  IMAGE=<registry>/cilium-custom-node-exporter:0.1.0
```

把 `deploy/daemonset.yaml` 的 `image` 換成上面的 image 後：

```bash
kubectl apply -f deploy/daemonset.yaml
kubectl -n cilium rollout status ds/cilium-custom-node-exporter
curl -s http://<node IP>:9969/metrics | grep ^cilium_policy
```

DaemonSet 的重點：

- `hostNetwork: true`，listen 在節點的 9969 port，節點的 Cilium datapath 出問題時一樣能被 scrape。
- `privileged: true`，唯讀掛載 host 的 `/sys/fs/bpf`（`mountPropagation: HostToContainer`）和 `/var/run/cilium`。
- `tolerations: [{operator: Exists}]`，包含 control-plane 在內所有節點都會跑。
- 不使用 ServiceAccount token，不需要 RBAC。
- `cilium` namespace 要先存在，YAML 不會建立或刪除它；有開 Pod Security Admission 的話要允許 privileged Pod。

接 Prometheus：Pod 帶有 `prometheus.io/scrape` annotation；用 Prometheus Operator 的話套用 `deploy/servicemonitor.yaml` 和 `deploy/prometheusrule.yaml`（還沒有 Prometheus 的話再加 repo 根目錄的 `deploy/prometheus.yaml`）。scrape 設定要開 `honor_labels: true`，不然 `namespace`、`pod` 會被 exporter 自己的 Pod 蓋掉。

## 疑難排解

| 現象 | 可能原因 |
|---|---|
| 沒有任何 `cilium_policy_map_entries` | cilium-agent 還沒起來，或 bpffs 不在 `/sys/fs/bpf`；看 `errors_total{stage="list_maps"}` 和 log |
| `endpoint_type="unknown"`、`pod` 是空的 | cilium-agent 查不到這個 endpoint；檢查 `cilium_api_up` |
| `cilium_api_up` 為 0 | 連不上 `/var/run/cilium/cilium.sock`，看 log |
| `errors_total{stage="read_map"}` 持續增加 | 權限不足（確認 `privileged: true`）或 map 格式不符 |
| `time() - last_collect_timestamp_seconds` 遠大於 collect interval | 背景讀取沒有在跑，看 log |

## 測試

```bash
make test   # 以 root 執行才會跑 BPF 相關測試，否則 skip
```
