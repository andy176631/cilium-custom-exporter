// Command cilium-custom-cluster-exporter exports the number of CiliumIdentity
// objects in the cluster, per namespace, and the number of distinct values of
// each security label key. It watches
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

func main() {
	var (
		listenAddr      = flag.String("listen-address", ":9970", "Address to serve /metrics, /healthz and /readyz on.")
		kubeconfig      = flag.String("kubeconfig", "", "Path to a kubeconfig. Empty means the in-cluster service account.")
		collectInterval = flag.Duration("collect-interval", 30*time.Second, "How often to aggregate the cached CiliumIdentity objects. Scrapes are served from the last result.")
		topNamespaces   = flag.Int("distinct-values-top-namespaces", 10, "Export cilium_identity_label_distinct_values only for this many namespaces with the most identities (by cilium_identity_count). 0 means every namespace.")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if *collectInterval <= 0 {
		slog.Error("--collect-interval must be positive", "value", *collectInterval)
		os.Exit(1)
	}
	if *topNamespaces < 0 {
		slog.Error("--distinct-values-top-namespaces must not be negative", "value", *topNamespaces)
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

	collector := newIdentityCollector(informer.GetStore(), informer.HasSynced, *topNamespaces)
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

	slog.Info("Serving metrics", "addr", *listenAddr, "collect_interval", *collectInterval, "distinct_values_top_namespaces", *topNamespaces)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server failed", "err", err)
		os.Exit(1)
	}
}
