// Command cilium-custom-cluster-exporter exports the number of CiliumIdentity
// objects in the cluster, per namespace and per security label. It watches
// CiliumIdentity objects through the Kubernetes API and is meant to run as a
// single-replica Deployment.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// defaultExcludeKeys are labels that change with every rollout or job run, so
// each value lives only as long as one ReplicaSet or Job. Cilium already
// leaves pod-template-hash and controller-revision-hash out of identities by
// default; these are the ones it keeps.
var defaultExcludeKeys = []string{
	"k8s:rollouts-pod-template-hash",
	"k8s:pod-template-hash",
	"k8s:controller-revision-hash",
	"k8s:pod-template-generation",
	"k8s:controller-uid",
	"k8s:batch.kubernetes.io/controller-uid",
	"k8s:job-name",
	"k8s:batch.kubernetes.io/job-name",
}

func main() {
	var (
		listenAddr      = flag.String("listen-address", ":9970", "Address to serve /metrics, /healthz and /readyz on.")
		kubeconfig      = flag.String("kubeconfig", "", "Path to a kubeconfig. Empty means the in-cluster service account.")
		collectInterval = flag.Duration("collect-interval", 30*time.Second, "How often to aggregate the cached CiliumIdentity objects. Scrapes are served from the last result.")
		excludeKeys     = flag.String("label-exclude-keys", strings.Join(defaultExcludeKeys, ","), "Comma-separated security label keys never exported as cilium_identity_security_label series. They are still counted in cilium_identity_label_distinct_values.")
		maxValues       = flag.Int("label-max-values", 50, "Leave a security label key out of cilium_identity_security_label in a namespace once it has more distinct values than this. 0 means no limit.")
		identityInfo    = flag.Bool("identity-info", false, "Also export cilium_identity_info, one series per identity with all its security labels. Off by default because it adds a series per identity and churns on every rollout.")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if *collectInterval <= 0 {
		slog.Error("--collect-interval must be positive", "value", *collectInterval)
		os.Exit(1)
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		slog.Error("Loading Kubernetes client config", "err", err)
		os.Exit(1)
	}
	cfg.UserAgent = "cilium-custom-cluster-exporter"
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		slog.Error("Creating Kubernetes client", "err", err)
		os.Exit(1)
	}

	// CiliumIdentity objects are watched, not listed per scrape: after the
	// initial list the API server only sends changes.
	factory := dynamicinformer.NewDynamicSharedInformerFactory(client, 0)
	informer := factory.ForResource(identityGVR).Informer()
	if err := informer.SetTransform(stripIdentity); err != nil {
		slog.Error("Setting informer transform", "err", err)
		os.Exit(1)
	}

	opts := aggregateOptions{excludeKeys: map[string]bool{}, maxValuesPerKey: *maxValues}
	for _, k := range strings.Split(*excludeKeys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			opts.excludeKeys[k] = true
		}
	}
	collector := newIdentityCollector(informer.GetStore(), informer.HasSynced, opts, *identityInfo)
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collector,
	)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		ErrorHandling: promhttp.ContinueOnError,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	// Ready once the first snapshot exists, so a rollout does not replace a
	// working pod with one that has nothing to report yet.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if collector.last.Load() == nil {
			http.Error(w, "CiliumIdentity cache not synced\n", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: *listenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	factory.Start(ctx.Done())
	go func() {
		if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			return
		}
		slog.Info("CiliumIdentity cache synced", "identities", len(informer.GetStore().ListKeys()))
		collector.refresh()
		collector.run(ctx, *collectInterval)
	}()

	slog.Info("Serving metrics", "addr", *listenAddr, "collect_interval", *collectInterval, "label_max_values", *maxValues, "identity_info", *identityInfo)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server failed", "err", err)
		os.Exit(1)
	}
}
