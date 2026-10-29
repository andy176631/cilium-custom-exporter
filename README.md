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

## GitLab CI 發版（依 tag 前綴打包並推到 Harbor）

[`.gitlab-ci.yml`](.gitlab-ci.yml) 只在推 tag 時跑，tag 的前綴決定要打包哪一個 exporter，兩個 exporter 可以各自發版：

| Tag | 打包的目錄 | 推上去的 image |
|---|---|---|
| `node-exporter/v1.2.0` | `cilium-custom-node-exporter/` | `<harbor>/<project>/cilium-custom-node-exporter:v1.2.0` |
| `cluster-exporter/v1.0.0` | `cilium-custom-cluster-exporter/` | `<harbor>/<project>/cilium-custom-cluster-exporter:v1.0.0` |

版本必須是 `vX.Y.Z`，可以帶 pre-release 後綴（例如 `v1.2.0-rc.1`）。正式版會同時推 `latest`，pre-release 不會。不符合格式的 tag 和一般的 push 都不會跑任何 job。

每次發版依序跑三個 stage：

1. **test**：在該目錄跑 `go vet ./...` 和 `go test ./...`。
2. **build**：用 Kaniko 以該目錄為 context build Dockerfile，一次推到設定檔裡的**每一個** Harbor。Kaniko 不需要 privileged runner。
3. **release**：建立 GitLab Release，並把該元件 `deploy/*.yaml` 裡的 image 換成新版本（取清單第一個 Harbor），連同 `images.txt` 一起放在 job artifacts，Release 頁面有連結。

### 要編輯的設定檔

所有設定都在 [`ci/release-config.yml`](ci/release-config.yml)，`.gitlab-ci.yml` 不用動：

```yaml
.runner:
  tags:            # 要跟 runner 註冊時設定的 tags 一樣
    - docker

variables:
  HARBOR_REGISTRIES: |   # 一行一個：<harbor host>/<project>  <帳密變數前綴>
    harbor-a.example.com/cilium  HARBOR_A
    harbor-b.example.com/cilium  HARBOR_B
  PUSH_LATEST: "true"
```

**密碼和 token 不要寫進這個檔案**（它會被 commit）。每個 Harbor 的帳密放在 GitLab 的 **Settings → CI/CD → Variables**，建議勾 Masked 和 Protected，並使用 Harbor robot account：

| CI/CD 變數 | 內容 |
|---|---|
| `HARBOR_A_USER` / `HARBOR_A_PASSWORD` | 清單中前綴為 `HARBOR_A` 的 Harbor 帳密 |
| `HARBOR_B_USER` / `HARBOR_B_PASSWORD` | 前綴為 `HARBOR_B` 的 Harbor 帳密 |
| `HARBOR_CA_CERT`（選填，類型選 File） | Harbor 使用自簽憑證時的 CA（PEM） |

若變數設成 Protected，記得到 **Settings → Repository → Protected tags** 把 `node-exporter/*`、`cluster-exporter/*` 設為 protected，否則 tag pipeline 讀不到這些變數。同一個 Harbor host 只能有一組帳密（docker config 以 host 為 key），同 host 的多個 project 請用同一個前綴。

`GO_IMAGE`、`KANIKO_IMAGE`、`RELEASE_CLI_IMAGE` 也在同一個檔案，runner 連不到外網時改成內部 mirror 即可。

### GitLab runner

Runner 本身的連線資訊（GitLab URL、runner authentication token）是放在 runner 主機上的 `/etc/gitlab-runner/config.toml`，不在 repo 裡。範本在 [`ci/runner/config.toml.example`](ci/runner/config.toml.example)：

1. 在 GitLab **Settings → CI/CD → Runners → New project runner** 建立 runner，設定 tags（例如 `docker`），取得 `glrt-...` token。
2. 在 runner 主機上依範本填入 `url` 和 `token`（或用範本註解中的 `gitlab-runner register` 指令），executor 用 docker 或 kubernetes 都可以，不需要 privileged。
3. 確認 `ci/release-config.yml` 的 `.runner.tags` 和步驟 1 的 tags 一致。

### 發版

```bash
git tag node-exporter/v1.2.0
git push origin node-exporter/v1.2.0

git tag cluster-exporter/v1.0.0
git push origin cluster-exporter/v1.0.0
```
