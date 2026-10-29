# cilium-custom-exporter

Cilium 的自訂 Prometheus exporter（支援 Cilium 1.13 / 1.14 / 1.16；cluster exporter 需要 identity allocation mode 為 `crd`），分成兩個獨立的程式，各自有自己的 Go module、Dockerfile 和部署 YAML：

| 目錄 | 部署方式 | 資料來源 | 匯出內容 |
|---|---|---|---|
| [`cilium-custom-node-exporter/`](cilium-custom-node-exporter/README.md) | DaemonSet（每個節點一個） | 節點上 pinned 的 BPF map、cilium-agent API socket | 每個 endpoint policy BPF map 的 entry 數量（帶 `pod`、`namespace`、`node`） |
| [`cilium-custom-cluster-exporter/`](cilium-custom-cluster-exporter/README.md) | Deployment（整個 cluster 一個） | Kubernetes API server 的 `CiliumIdentity` | identity 總數、每個 namespace 的數量、每個 security label key 的不同值數量 |

`cilium-custom-node-exporter` 就是原本的 `cilium-custom-exporter`，改名後移到子目錄。它自己的 metric（`cilium_custom_exporter_*`）也一起改成 `cilium_custom_node_exporter_*`，`cilium_policy_map_*` 不變。

兩個 exporter 的資源都放在 `cilium` namespace（要先存在，YAML 不會建立或刪除它），只有 ServiceMonitor 放在 `monitoring` namespace，而且都帶 `app.kubernetes.io/part-of: cilium-custom-exporter` label。

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

## GitLab CI 打包並推到 Harbor

[`.gitlab-ci.yml`](.gitlab-ci.yml) 只在推 tag 時執行。tag 的前綴決定要打包哪一個 exporter，`/` 後面的部分就是 image tag：

| Tag | 打包的目錄 | 推上去的 image |
|---|---|---|
| `node-exporter/v1.2.0` | `cilium-custom-node-exporter/` | `<harbor>/cilium-custom-node-exporter:v1.2.0` |
| `cluster-exporter/v1.0.0` | `cilium-custom-cluster-exporter/` | `<harbor>/cilium-custom-cluster-exporter:v1.0.0` |

image tag 要符合 Docker tag 的規則（英數字、`_`、`.`、`-`，最多 128 字元）。其他 tag 和一般的 push 都不會執行任何 job。

### Harbor 清單

build server 要推到哪些 Harbor 寫在 [`ci/harbor-registries.txt`](ci/harbor-registries.txt)，一行一個 `<harbor host>/<project>`，image 會推到每一個位置：

```
harbor-a.example.com/cilium
harbor-b.example.com/cilium
```

### Build server（透過 SSH）

Runner 本身不會 build。它會用 SSH 把該 exporter 的原始碼複製到 build server 的 `/tmp/gitlab-build/` 底下，在那台機器上執行 [`ci/remote-build.sh`](ci/remote-build.sh) 做 `docker build`，再 push 到每個 Harbor。完成後會刪除暫存目錄和本機 image。

- **Build server**：要有 docker，而且已經對清單上的每個 Harbor 執行過 `docker login`。SSH 使用者要能執行 docker。
- **Runner**：要有 `git`、`ssh`、`tar`（例如 shell executor），並且連得到 build server。runner 的 tags 要和 `.gitlab-ci.yml` 中 `default.tags` 的值一致（預設是 `harbor`）。

連線資訊放在 GitLab **Settings → CI/CD → Variables**：

| 變數 | 必填 | 內容 |
|---|---|---|
| `BUILD_SERVER_HOST` | 是 | build server 的 IP 或 hostname |
| `BUILD_SERVER_SSH_KEY` | 是 | SSH private key。建議類型選 **File**，也可以是一般 Variable |
| `BUILD_SERVER_USER` | 否 | SSH 使用者，預設 `root` |
| `BUILD_SERVER_PORT` | 否 | SSH port，預設 `22` |
| `BUILD_SERVER_KNOWN_HOSTS` | 否 | build server 的 known_hosts 內容（`ssh-keyscan <ip>` 的輸出）。有設定就嚴格檢查 host key；沒設定則第一次連線時直接接受 |

變數若設成 Protected，要到 **Settings → Repository → Protected tags** 把 `node-exporter/*`、`cluster-exporter/*` 設為 protected，否則 tag pipeline 讀不到這些變數。

### 發版

```bash
git tag node-exporter/v1.2.0 && git push origin node-exporter/v1.2.0
git tag cluster-exporter/v1.0.0 && git push origin cluster-exporter/v1.0.0
```
