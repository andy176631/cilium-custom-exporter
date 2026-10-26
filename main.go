// Command cilium-custom-exporter exports the number of entries in every
// Cilium endpoint policy BPF map on a node, labelled with the pod that owns
// the endpoint. It is meant to run as a DaemonSet next to cilium-agent.
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
)

func main() {
	var (
		listenAddr      = flag.String("listen-address", ":9969", "Address to serve /metrics and /healthz on.")
		collectInterval = flag.Duration("collect-interval", 30*time.Second, "How often to read the policy maps in the background. Scrapes are served from the last result and never read the maps themselves.")
		mapDir          = flag.String("bpf-map-dir", "/sys/fs/bpf/tc/globals", "Directory where cilium-agent pins its BPF maps.")
		socket          = flag.String("cilium-socket", "/var/run/cilium/cilium.sock", "Path to the cilium-agent API socket.")
		nodeName        = flag.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name. Defaults to $NODE_NAME, then the hostname.")
		cacheTTL        = flag.Duration("endpoint-cache-ttl", 30*time.Second, "How long to reuse the endpoint list from cilium-agent. A new endpoint ID always triggers a refresh.")
		apiTimeout      = flag.Duration("cilium-api-timeout", 5*time.Second, "Timeout for requests to the cilium-agent API.")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if *collectInterval <= 0 {
		slog.Error("--collect-interval must be positive", "value", *collectInterval)
		os.Exit(1)
	}
	if *nodeName == "" {
		h, err := os.Hostname()
		if err != nil {
			slog.Error("No --node-name or $NODE_NAME, and the hostname is unavailable", "err", err)
			os.Exit(1)
		}
		*nodeName = h
	}

	endpoints := &endpointCache{client: newCiliumClient(*socket), ttl: *cacheTTL}
	collector := newPolicyMapCollector(*nodeName, *mapDir, endpoints, *apiTimeout)
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
	srv := &http.Server{Addr: *listenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	// Read the maps once before serving so the first scrape already has data.
	collector.refresh(ctx)
	go collector.run(ctx, *collectInterval)

	slog.Info("Serving metrics", "addr", *listenAddr, "node", *nodeName, "collect_interval", *collectInterval, "bpf_map_dir", *mapDir, "cilium_socket", *socket)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server failed", "err", err)
		os.Exit(1)
	}
}
