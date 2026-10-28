# cilium-custom-cluster-exporter

> 這是 [cilium-custom-exporter](../README.md) 的 cluster 層 exporter。以下指令都在 `cilium-custom-cluster-exporter/` 目錄下執行。

以單一 replica 的 Deployment 執行，透過 Kubernetes API server 讀取所有 `CiliumIdentity`（`cilium.io/v2`），匯出 identity 總數、每個 namespace 的數量、security label 的分布，以及每個 label key 有幾種不同的值。

支援環境：Cilium 1.13 / 1.14，identity allocation mode 為 `crd`（預設值）。用 kvstore 模式的話，identity 不在 Kubernetes API 裡，這個 exporter 看不到。

## Metrics

| Metric | 類型 | Label | 說明 |
|---|---|---|---|
| `cilium_identity_total` | gauge | | cluster 內 CiliumIdentity 的總數 |
| `cilium_identity_count` | gauge | `namespace` | 每個 namespace 的 CiliumIdentity 數量 |
| `cilium_identity_security_label` | gauge | `namespace`, `key`, `value` | 該 namespace 裡，security labels 含有 `key=value` 的 identity 數量 |
| `cilium_identity_label_distinct_values` | gauge | `namespace`, `key` | 該 namespace 裡，這個 label key 在所有 identity 中有幾種不同的值 |
| `cilium_identity_info` | gauge | `identity`, `namespace`, `labels` | 每個 identity 一筆，值固定為 1，`labels` 是完整的 security labels。**預設關閉**，見下方說明 |
| `cilium_custom_cluster_exporter_cache_synced` | gauge | | 第一次從 API server list 完成前是 0。0 的期間不會匯出任何 identity metric，避免把「還沒讀完」當成「identity 變少」 |
| `cilium_custom_cluster_exporter_suppressed_label_keys` | gauge | | 有幾組 namespace/key 因為被排除或值太多，沒有展開成 `cilium_identity_security_label` |
| `cilium_custom_cluster_exporter_collect_duration_seconds` | gauge | | 最近一次彙整花的時間 |
| `cilium_custom_cluster_exporter_last_collect_timestamp_seconds` | gauge | | 最近一次彙整完成的時間（Unix 秒） |

`namespace` 取自 identity 的 `k8s:io.kubernetes.pod.namespace` security label（沒有的話用 `metadata.labels` 的 `io.kubernetes.pod.namespace`）。不屬於任何 namespace 的 identity，`namespace` 是空字串。`key` 保留 Cilium 的來源前綴，例如 `k8s:app`。namespace label 本身已經是每筆 series 的 `namespace`，所以不會再出現在 `key` 裡。

範例輸出：

```
cilium_identity_total 40
cilium_identity_count{namespace="default"} 3
cilium_identity_count{namespace="payments"} 37
cilium_identity_label_distinct_values{namespace="payments",key="k8s:app"} 2
cilium_identity_label_distinct_values{namespace="payments",key="k8s:rollouts-pod-template-hash"} 37
cilium_identity_security_label{namespace="payments",key="k8s:app",value="api"} 30
cilium_identity_security_label{namespace="payments",key="k8s:app",value="worker"} 7
cilium_identity_security_label{namespace="payments",key="k8s:io.cilium.k8s.policy.serviceaccount",value="default"} 37
cilium_custom_cluster_exporter_suppressed_label_keys 1
```

## Security label 的基數（cardinality）

Prometheus 的成本跟 series 數量成正比，而 security label 是這個 exporter 最容易爆量的地方。設計上的取捨：

1. **預設不做「每個 identity 一筆」**。最直覺的做法是 `cilium_identity_info{identity="20001", labels="..."}`，但 identity 數量本身就可能上萬，而且每次 rollout 都會產生新的 identity ID、舊的被回收，series 會不斷換新，Prometheus 的 head block 和 index 會一直膨脹。所以這個做法放在 `--identity-info` 後面，預設關閉，只建議在 identity 數量少、或短時間除錯時打開。
2. **改用「每個 namespace/key/value 一筆，值是 identity 數量」**（`cilium_identity_security_label`）。series 數量取決於「有幾種不同的 label 值」而不是「有幾個 identity」，同一個 Deployment 換版不會增加 series，回答的問題也比較實用，例如「payments 裡有幾個 identity 帶 `app=api`」。
3. **會隨每次 rollout 改變的 key 不展開值**。像 Argo Rollouts 的 `rollouts-pod-template-hash`、Job 的 `job-name` / `controller-uid`，每個值只活一個 ReplicaSet 或一次 Job，展開就等於回到第 1 點的問題。這些 key 列在 `--label-exclude-keys`（預設清單見[參數](#參數)），仍然會計入 `cilium_identity_label_distinct_values`。
4. **自動上限**：就算 key 沒有被排除，只要某個 namespace 裡這個 key 的不同值超過 `--label-max-values`（預設 50），就不展開它的值。被跳過的組數看 `cilium_custom_cluster_exporter_suppressed_label_keys`。
5. **`cilium_identity_label_distinct_values` 永遠完整**。它每個 namespace/key 只有一筆，基數很低，正好用來找出「是哪個 label 讓 identity 變多」。例如 `cilium_identity_label_distinct_values{namespace="payments", key="k8s:rollouts-pod-template-hash"} 37` 代表 payments 因為 rollouts hash 多出了 37 種 identity；這種 label 通常應該加進 Cilium 的 `labels` 設定（identity-relevant labels）排除掉。

估算：series 數量大約是「所有 namespace 中，未被排除的 key 的不同值總數」加上「namespace 數 × key 數」。一般叢集是幾百到幾千筆。

## 運作方式

1. **Informer 快取**：啟動時對 `ciliumidentities` 做一次 list，之後用 watch 接收變更，API server 只會送有變動的 identity。scrape 時不會呼叫 API server，Prometheus 多頻繁 scrape 都不影響 API server 的負載。
2. **只留需要的欄位**：informer 存進記憶體前會把 identity 縮成只剩名稱和 `security-labels`，丟掉 `managedFields` 等 metadata，記憶體用量大致只跟 identity 和 label 的數量成正比。
3. **背景彙整**：每 `--collect-interval`（預設 30 秒）把快取彙整成 metrics，scrape 只回傳最近一次的結果，跟 node exporter 的背景快取是同一個做法。數字最多比實際晚一個 collect interval。
4. **啟動與 API server 斷線**：第一次 list 完成前 `/readyz` 回 503、只匯出 `cache_synced 0`。之後如果 watch 斷線，client-go 會自動重連並重新 list，期間沿用快取裡的資料。

## 建置 image

```bash
make image IMAGE=<你的 registry>/cilium-custom-cluster-exporter:0.1.0
make push  IMAGE=<你的 registry>/cilium-custom-cluster-exporter:0.1.0
```

產出的是靜態連結的 amd64 binary，base image 是 `gcr.io/distroless/static-debian12:nonroot`。

## 部署

1. 把 `deploy/deployment.yaml` 裡的 `image: registry.example.com/cilium-custom-cluster-exporter:0.1.0` 換成你推上去的 image。
2. 套用：

```bash
kubectl apply -f deploy/deployment.yaml
kubectl -n cilium rollout status deploy/cilium-custom-cluster-exporter
```

`deploy/deployment.yaml` 包含：

- `ServiceAccount` `cilium-custom-cluster-exporter`（namespace `cilium`）。
- `ClusterRole` / `ClusterRoleBinding`：只有 `cilium.io` 的 `ciliumidentities` 的 `get`、`list`、`watch`。CiliumIdentity 是 cluster-scoped 資源，所以必須是 ClusterRole；exporter 不會修改任何 identity。
- `Deployment`：1 個 replica（metrics 描述整個 cluster，兩個 replica 只會讓 Prometheus 存兩份）、非 root、唯讀 root filesystem、drop 所有 capabilities，不需要 hostNetwork 或 privileged。listen 在 `9970` port。

跟 node exporter 一樣，所有 namespaced 資源都放在 `cilium` namespace，YAML 不會建立或刪除這個 namespace。

確認輸出：

```bash
kubectl -n cilium port-forward deploy/cilium-custom-cluster-exporter 9970
curl -s localhost:9970/metrics | grep ^cilium_identity
```

## 接 Prometheus

- **用 annotation 自動發現 Pod**：Deployment 已經帶 `prometheus.io/scrape: "true"`、`prometheus.io/port: "9970"`。
- **Prometheus Operator**：套用 `deploy/servicemonitor.yaml`。repo 根目錄的 `deploy/prometheus.yaml` 會用 `app.kubernetes.io/part-of: cilium-custom-exporter` 同時選到 node 和 cluster 兩個 exporter 的 ServiceMonitor。

不管哪一種，都要開 `honor_labels: true`（ServiceMonitor 範例已經設好），不然 metric 的 `namespace` label（identity 所屬的 namespace）會被 exporter Pod 自己的 `cilium` 蓋掉，原本的值變成 `exported_namespace`。

## PromQL 範例

```promql
# identity 最多的 10 個 namespace
topk(10, cilium_identity_count)

# 哪些 label 讓 identity 變多（不同值最多的 key）
topk(10, cilium_identity_label_distinct_values)

# 每個 namespace 裡 app=api 的 identity 數
cilium_identity_security_label{key="k8s:app", value="api"}

# 一小時內 identity 增加超過 500 個
delta(cilium_identity_total[1h]) > 500
```

## 參數

| 參數 | 預設值 | 說明 |
|---|---|---|
| `--listen-address` | `:9970` | `/metrics`、`/healthz`、`/readyz` 的 listen 位址 |
| `--kubeconfig` | 空（用 in-cluster ServiceAccount） | 在叢集外測試時指定 kubeconfig |
| `--collect-interval` | `30s` | 背景彙整快取的間隔 |
| `--label-exclude-keys` | `k8s:rollouts-pod-template-hash,k8s:pod-template-hash,k8s:controller-revision-hash,k8s:pod-template-generation,k8s:controller-uid,k8s:batch.kubernetes.io/controller-uid,k8s:job-name,k8s:batch.kubernetes.io/job-name` | 不展開成 `cilium_identity_security_label` 的 key（逗號分隔），仍會計入 `cilium_identity_label_distinct_values` |
| `--label-max-values` | `50` | 某個 namespace 裡一個 key 的不同值超過這個數就不展開；0 表示不限制 |
| `--identity-info` | `false` | 匯出 `cilium_identity_info`（每個 identity 一筆） |

## 開發與測試

```bash
make test
```

測試內容：

- 彙整邏輯：總數、每個 namespace、distinct values、排除 key 與數量上限。
- informer transform：縮減欄位、namespace 從 `metadata.labels` 補、tombstone 原樣通過。
- 用 fake API 跑 informer 到 `/metrics`：37 個帶不同 `rollouts-pod-template-hash` 的 identity 得到 `distinct_values 37`，刪除 identity 後下一次彙整數字跟著變。

另外在真的 kube-apiserver（v1.31，envtest）上套用 Cilium 1.14 的 CiliumIdentity CRD 和 `deploy/deployment.yaml`，以 exporter 的 ServiceAccount 身分執行：ClusterRole 足夠 list/watch、不能刪除；拿掉 ClusterRoleBinding 時 `/readyz` 維持 503。
