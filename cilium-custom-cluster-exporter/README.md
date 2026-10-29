# cilium-custom-cluster-exporter

以單一 replica 的 Deployment 執行的 Prometheus exporter，透過 Kubernetes API server 讀取所有 `CiliumIdentity`（`cilium.io/v2`），匯出 **identity 總數**、**每個 namespace 的 identity 數**，以及 **identity 數前 N 名的 namespace 裡，每個 security label key 有幾種不同的值**。用來觀察 identity 的成長，並找出是哪個 label 讓 identity 變多。

- 支援版本：Cilium 1.13、1.14，identity allocation mode 為 `crd`（預設值）。kvstore 模式的 identity 不在 Kubernetes API 裡，這個 exporter 看不到
- 部署：`deploy/deployment.yaml`，namespace `cilium`
- 以下指令都在 `cilium-custom-cluster-exporter/` 目錄下執行；整個 repo 的介紹見[上層 README](../README.md)

## 運作機制

```
 kube-apiserver ──(1) list + watch ciliumidentities──► informer 快取（只留名稱和 security-labels）
                                                              │
                                              (2) 每 --collect-interval 彙整
                                                              ▼
                                                      記憶體快照 ──(3)──► /metrics
```

1. **Informer 快取**：啟動時對 `ciliumidentities` 做一次 list，之後用 watch 接收新增、修改、刪除，API server 只會送有變動的 identity。存進快取前會把每個 identity 縮成只剩名稱和 `security-labels`，丟掉 `managedFields` 等 metadata，減少記憶體用量。watch 斷線時 client-go 會自動重連並重新 list，期間沿用快取裡的資料。
2. **背景彙整**：快取完成第一次 list 後，每 `--collect-interval`（預設 30 秒）彙整一次：
   - 算出 identity 總數和每個 namespace 的數量。namespace 取自 security label `k8s:io.kubernetes.pod.namespace`，沒有的話用 `metadata.labels` 的 `io.kubernetes.pod.namespace`，兩者都沒有的 identity 歸在空字串 namespace。
   - 依數量排序 namespace（同數量依名稱），只對前 `--distinct-values-top-namespaces` 名，計算每個 security label key 有幾種不同的值。namespace label 本身不計入。
3. **回應 scrape**：Prometheus scrape 時只回傳最近一次彙整的結果，不會呼叫 API server，所以 scrape 頻率不影響 API server 的負載。數字最多比實際晚一個 collect interval。

第一次 list 完成前，`/readyz` 回 503，`/metrics` 只有 `cilium_custom_cluster_exporter_cache_synced 0`，不會把「還沒讀完」當成「identity 變少」匯出。

## Metrics

| Metric | 類型 | Label | 說明 |
|---|---|---|---|
| `cilium_identity_total` | gauge | （無） | cluster 內 CiliumIdentity 的總數 |
| `cilium_identity_count` | gauge | `namespace` | 每個 namespace 的 CiliumIdentity 數量，包含所有 namespace |
| `cilium_identity_label_distinct_values` | gauge | `namespace`, `key` | 該 namespace 裡，這個 security label key 在所有 identity 中有幾種不同的值。只匯出 `cilium_identity_count` 前 N 名的 namespace（`--distinct-values-top-namespaces`） |
| `cilium_custom_cluster_exporter_cache_synced` | gauge | （無） | 快取是否已完成第一次 list 並彙整過（1 / 0） |
| `cilium_custom_cluster_exporter_collect_duration_seconds` | gauge | （無） | 最近一次彙整花的時間 |
| `cilium_custom_cluster_exporter_last_collect_timestamp_seconds` | gauge | （無） | 最近一次彙整完成的時間（Unix 秒） |

Label 說明：

| Label | 內容 |
|---|---|
| `namespace` | identity 所屬的 namespace；不屬於任何 namespace 的 identity 為空字串 |
| `key` | security label 的 key，保留 Cilium 的來源前綴，例如 `k8s:app`、`k8s:rollouts-pod-template-hash` |

範例輸出：

```
cilium_identity_total 40
cilium_identity_count{namespace="default"} 3
cilium_identity_count{namespace="payments"} 37
cilium_identity_label_distinct_values{namespace="payments",key="k8s:app"} 2
cilium_identity_label_distinct_values{namespace="payments",key="k8s:rollouts-pod-template-hash"} 37
```

上面的 `k8s:rollouts-pod-template-hash` 是 37，代表 payments 因為 Argo Rollouts 的 hash 多出了 37 種 identity。這種 label 通常應該在 Cilium 的 `labels` 設定（identity-relevant labels）裡排除掉。要看實際的值，用 `kubectl get ciliumidentities` 或 `cilium identity list`。

**Series 數量**：`cilium_identity_label_distinct_values` 只匯出值的「數量」而不是值本身，所以每次部署都會變的 label 不會讓 series 增加；又只取前 N 名 namespace，所以上限大約是 N × 每個 namespace 的 label key 數。前 N 名的組成改變時，掉出去的 namespace 的 series 會消失、新進來的會出現。

PromQL 範例：

```promql
# identity 最多的 10 個 namespace
topk(10, cilium_identity_count)

# 不同值最多的 label key
topk(10, cilium_identity_label_distinct_values)

# 一小時內 identity 增加超過 500 個
delta(cilium_identity_total[1h]) > 500
```

## 參數

| 參數 | 型別 | 預設值 | 說明 |
|---|---|---|---|
| `--listen-address` | string | `:9970` | `/metrics`、`/healthz`、`/readyz` 的 listen 位址 |
| `--kubeconfig` | string | 空 | kubeconfig 路徑。空字串表示用 in-cluster ServiceAccount（在叢集內執行時用這個）；在叢集外測試時才需要指定 |
| `--collect-interval` | duration | `30s` | 背景彙整快取的間隔，必須大於 0。scrape 只回傳最近一次的結果 |
| `--distinct-values-top-namespaces` | int | `10` | 只對 `cilium_identity_count` 前 N 名的 namespace 匯出 `cilium_identity_label_distinct_values`。0 表示全部 namespace，不可為負數 |

duration 的格式是 Go 的 `time.ParseDuration`，例如 `15s`、`1m`。

HTTP endpoint：`/metrics`（Prometheus 格式）、`/healthz`（固定回 `ok`，給 liveness probe）、`/readyz`（第一次彙整完成前回 503，給 readiness probe）。

## 建置與部署

```bash
make image IMAGE=<registry>/cilium-custom-cluster-exporter:0.1.0
make push  IMAGE=<registry>/cilium-custom-cluster-exporter:0.1.0
```

把 `deploy/deployment.yaml` 的 `image` 換成上面的 image 後：

```bash
kubectl apply -f deploy/deployment.yaml
kubectl -n cilium rollout status deploy/cilium-custom-cluster-exporter
kubectl -n cilium port-forward deploy/cilium-custom-cluster-exporter 9970
curl -s localhost:9970/metrics | grep ^cilium_identity
```

`deploy/deployment.yaml` 包含：

- `ServiceAccount` `cilium-custom-cluster-exporter`。
- `ClusterRole` / `ClusterRoleBinding`：只有 `cilium.io` `ciliumidentities` 的 `get`、`list`、`watch`。CiliumIdentity 是 cluster-scoped 資源，所以必須是 ClusterRole；exporter 不會修改任何 identity。
- `Deployment`：1 個 replica（metrics 描述整個 cluster，多個 replica 只會讓 Prometheus 存多份）、非 root、唯讀 root filesystem、drop 所有 capabilities，不需要 hostNetwork 或 privileged。
- `cilium` namespace 要先存在，YAML 不會建立或刪除它。

接 Prometheus：Pod 帶有 `prometheus.io/scrape` annotation；用 Prometheus Operator 的話套用 `deploy/servicemonitor.yaml`（Service 放在 `cilium`，ServiceMonitor 放在 `monitoring` namespace 並用 `namespaceSelector` 選 `cilium` 裡的 Service；`monitoring` 要先存在）（還沒有 Prometheus 的話再加 repo 根目錄的 `deploy/prometheus.yaml`）。scrape 設定要開 `honor_labels: true`，不然 metric 的 `namespace`（identity 所屬的 namespace）會被 exporter Pod 自己的 `cilium` 蓋掉。

## 疑難排解

| 現象 | 可能原因 |
|---|---|
| `cache_synced` 一直是 0、Pod 不 ready | 沒有 list 權限（檢查 ClusterRoleBinding），或叢集沒有 `ciliumidentities` CRD（Cilium 用 kvstore 模式）；log 會有 `forbidden` 或 `not found` |
| 沒有 `cilium_identity_label_distinct_values` 的某個 namespace | 該 namespace 不在 identity 數前 N 名，調大 `--distinct-values-top-namespaces` 或設成 0 |
| `time() - last_collect_timestamp_seconds` 遠大於 collect interval | 背景彙整沒有在跑，看 log |

## 測試

```bash
make test
```

涵蓋彙整邏輯（總數、每個 namespace、distinct values、前 N 名 namespace 與同數量排序）、informer transform，以及用 fake API 跑 informer 到 `/metrics`（含刪除 identity 後數字更新）。
