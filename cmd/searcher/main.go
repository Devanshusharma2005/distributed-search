package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"github.com/Devanshusharma2005/distributed-search/internal/index"
	"github.com/blevesearch/bleve/v2"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics with shard labels
var (
	queryLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "search_query_latency_seconds",
		Help:    "Search query latency distribution",
		Buckets: prometheus.DefBuckets,
	}, []string{"status", "shard"})

	queriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "search_queries_total",
		Help: "Total search queries processed",
	}, []string{"status", "shard"})
)

func init() {
	prometheus.MustRegister(queryLatency)
	prometheus.MustRegister(queriesTotal)
}

var (
	shardID   = flag.Int("shard-id", -1, "shard ID (-1 = no etcd registration, single-node mode)")
	port      = flag.Int("port", 8080, "HTTP port")
	hostname  = flag.String("hostname", "localhost", "hostname to register in etcd (use container name in Docker)")
	etcdEps   = flag.String("etcd", "localhost:2379,localhost:2381,localhost:2383", "etcd endpoints (comma-separated)")
	indexBase = flag.String("index", "search.bleve", "base index path")
)

func main() {
	flag.Parse()

	indexPath := *indexBase
	if *shardID >= 0 {
		indexPath = fmt.Sprintf("%s-%d", *indexBase, *shardID)
	}

	idx, err := index.NewIndexer(indexPath)
	if err != nil {
		log.Fatalf("load index %s: %v", indexPath, err)
	}
	defer idx.Close()

	docCount, _ := idx.Index.DocCount()
	log.Printf("Shard service ready :%d (index=%s, docs=%d)", *port, indexPath, docCount)

	// etcd registration (only in shard mode)
	var cancelReg context.CancelFunc
	if *shardID >= 0 {
		ctx, cancel := context.WithCancel(context.Background())
		cancelReg = cancel
		go registerShard(ctx, *shardID, *port, *hostname, *etcdEps)
		log.Printf("Starting etcd registration for shard-%d...", *shardID)
	}

	// HTTP server setup
	r := mux.NewRouter()
	r.HandleFunc("/search", searchHandler(idx.Index, *shardID)).Methods("GET")
	r.Handle("/metrics", promhttp.Handler())
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}).Methods("GET")

	srv := &http.Server{
		Addr:    ":" + strconv.Itoa(*port),
		Handler: r,
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("HTTP listening on :%d", *port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	<-quit
	log.Println("Shutdown signal received...")
	if cancelReg != nil {
		cancelReg()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)

	log.Println("Shard service stopped")
}

// registerShard registers this shard in etcd with a keepalive lease
func registerShard(ctx context.Context, shardID, port int, hostname, etcdEps string) {
	eps := strings.Split(etcdEps, ",")

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   eps,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("etcd connect: %v", err)
	}
	defer cli.Close()

	lease, err := cli.Grant(ctx, 30)
	if err != nil {
		log.Fatalf("etcd lease: %v", err)
	}

	shardAddr := fmt.Sprintf("%s:%d", hostname, port)
	key := fmt.Sprintf("/shards/active/%d", shardID)

	_, err = cli.Put(ctx, key, shardAddr, clientv3.WithLease(lease.ID))
	if err != nil {
		log.Fatalf("etcd put %s: %v", key, err)
	}

	ch, kaErr := cli.KeepAlive(ctx, lease.ID)
	if kaErr != nil {
		log.Fatalf("etcd keepalive: %v", kaErr)
	}

	log.Printf("Shard-%d registered: %s → %s (lease=%d)", shardID, key, shardAddr, lease.ID)

	// Heartbeat loop - keeps the lease alive until context is cancelled
	for {
		select {
		case <-ctx.Done():
			log.Printf("Shard-%d deregistering from etcd...", shardID)
			return
		case ka, ok := <-ch:
			if !ok {
				log.Printf("Keepalive channel closed for shard-%d", shardID)
				return
			}
			if ka == nil {
				log.Printf("Keepalive failed for shard-%d, lease expired", shardID)
				return
			}
		}
	}
}

func searchHandler(idx bleve.Index, shardID int) http.HandlerFunc {
	shardLabel := "single"
	if shardID >= 0 {
		shardLabel = fmt.Sprintf("shard-%d", shardID)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		q := r.URL.Query().Get("q")
		if q == "" {
			http.Error(w, `{"error": "missing 'q' parameter"}`, http.StatusBadRequest)
			queriesTotal.WithLabelValues("error", shardLabel).Inc()
			queryLatency.WithLabelValues("error", shardLabel).Observe(time.Since(start).Seconds())
			return
		}

		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 20
		}

		query := bleve.NewQueryStringQuery(q)
		req := bleve.NewSearchRequest(query)
		req.Size = limit
		req.Fields = []string{"title", "body", "title_vector"}
		req.Highlight = bleve.NewHighlight()

		res, err := idx.Search(req)
		if err != nil {
			http.Error(w, `{"error": "`+err.Error()+`"}`, http.StatusInternalServerError)
			queriesTotal.WithLabelValues("error", shardLabel).Inc()
			queryLatency.WithLabelValues("error", shardLabel).Observe(time.Since(start).Seconds())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)

		queriesTotal.WithLabelValues("success", shardLabel).Inc()
		queryLatency.WithLabelValues("success", shardLabel).Observe(time.Since(start).Seconds())

		log.Printf("'%s' → %d hits in %v (shard=%s)", q, len(res.Hits), time.Since(start), shardLabel)
	}
}