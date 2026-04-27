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

	"github.com/Devanshusharma2005/distributed-search/internal/embed"
	"github.com/Devanshusharma2005/distributed-search/internal/hybrid"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	queryLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "coordinator_query_latency_seconds",
		Help:    "Query latency distribution",
		Buckets: prometheus.DefBuckets,
	}, []string{"endpoint", "status"})

	queriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "coordinator_queries_total",
		Help: "Total queries processed",
	}, []string{"endpoint", "status"})

	cacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_cache_hits_total",
		Help: "Total cache hits",
	})
	cacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "coordinator_cache_misses_total",
		Help: "Total cache misses",
	})
)

func init() {
	prometheus.MustRegister(queryLatency, queriesTotal, cacheHits, cacheMisses)
}

var (
	port      = flag.Int("port", 8090, "HTTP port")
	etcdEps   = flag.String("etcd", "localhost:2379,localhost:2381,localhost:2383", "etcd endpoints (comma-separated)")
	redisAddr = flag.String("redis", "localhost:6379", "Redis address")
	ollamaURL = flag.String("ollama", "http://localhost:11434", "Ollama API URL")
)

const (
	cacheTTL     = 5 * time.Minute
	ollamaModel  = "all-minilm"
	defaultLimit = 20
)

func main() {
	flag.Parse()

	log.Printf("Starting coordinator on :%d (etcd=%s, redis=%s, ollama=%s)",
		*port, *etcdEps, *redisAddr, *ollamaURL)

	rdb := redis.NewClient(&redis.Options{Addr: *redisAddr})
	defer rdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("Redis not reachable: %v (caching disabled)", err)
		rdb = nil
	} else {
		log.Printf("Redis connected at %s", *redisAddr)
	}
	cancel()

	var embedClient hybrid.EmbeddingClient
	ollamaClient := embed.NewOllamaClient(*ollamaURL, ollamaModel)

	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	_, err := ollamaClient.GetEmbedding(ctx, "test")
	cancel()
	if err != nil {
		log.Printf("Ollama not reachable: %v (hybrid search will use keyword-only)", err)
	} else {
		embedClient = ollamaClient
		log.Printf("Ollama connected at %s (model=%s)", *ollamaURL, ollamaModel)
	}

	searcher := hybrid.NewHybridSearcher(embedClient, *etcdEps)

	r := mux.NewRouter()
	r.HandleFunc("/search", searchHandler(searcher, rdb)).Methods("GET")
	r.HandleFunc("/hybrid", hybridHandler(searcher, rdb)).Methods("GET")
	r.HandleFunc("/shards", shardsHandler(*etcdEps)).Methods("GET")
	r.HandleFunc("/hot-terms", hotTermsHandler(*etcdEps)).Methods("GET")
	r.HandleFunc("/health", healthHandler).Methods("GET")
	r.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    ":" + strconv.Itoa(*port),
		Handler: r,
	}

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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}
	log.Println("Coordinator stopped")
}

func cacheKey(endpoint, query string, limit int) string {
	return fmt.Sprintf("%s:%s:%d", endpoint, query, limit)
}

func getFromCache(ctx context.Context, rdb *redis.Client, key string) ([]byte, bool) {
	if rdb == nil {
		return nil, false
	}
	val, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	return val, true
}

func setCache(ctx context.Context, rdb *redis.Client, key string, data []byte) {
	if rdb == nil {
		return
	}
	rdb.Set(ctx, key, data, cacheTTL)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

func writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := map[string]string{"error": msg}
	json.NewEncoder(w).Encode(resp)
}

func searchHandler(searcher *hybrid.HybridSearcher, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		q := r.URL.Query().Get("q")
		if q == "" {
			writeError(w, "missing 'q' parameter", http.StatusBadRequest)
			queriesTotal.WithLabelValues("search", "error").Inc()
			return
		}

		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = defaultLimit
		}

		key := cacheKey("search", q, limit)
		if cached, ok := getFromCache(r.Context(), rdb, key); ok {
			cacheHits.Inc()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(cached)
			queriesTotal.WithLabelValues("search", "success").Inc()
			queryLatency.WithLabelValues("search", "success").Observe(time.Since(start).Seconds())
			return
		}
		cacheMisses.Inc()

		resp, err := searcher.Search(r.Context(), q, limit, -1)
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			queriesTotal.WithLabelValues("search", "error").Inc()
			queryLatency.WithLabelValues("search", "error").Observe(time.Since(start).Seconds())
			return
		}

		data, _ := json.Marshal(resp)
		setCache(r.Context(), rdb, key, data)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(data)

		queriesTotal.WithLabelValues("search", "success").Inc()
		queryLatency.WithLabelValues("search", "success").Observe(time.Since(start).Seconds())

		log.Printf("SEARCH '%s' → %d hits in %v", q, len(resp.Hits), time.Since(start))
	}
}

func hybridHandler(searcher *hybrid.HybridSearcher, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		q := r.URL.Query().Get("q")
		if q == "" {
			writeError(w, "missing 'q' parameter", http.StatusBadRequest)
			queriesTotal.WithLabelValues("hybrid", "error").Inc()
			return
		}

		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 10
		}

		alpha := -1.0
		if alphaStr := r.URL.Query().Get("alpha"); alphaStr != "" {
			if parsed, err := strconv.ParseFloat(alphaStr, 64); err == nil {
				alpha = parsed
			}
		}

		fusionMethod := r.URL.Query().Get("fusion")
		if fusionMethod == "" {
			fusionMethod = "rrf"
		}

		key := cacheKey("hybrid", fmt.Sprintf("%s:%.2f:%s", q, alpha, fusionMethod), limit)
		if cached, ok := getFromCache(r.Context(), rdb, key); ok {
			cacheHits.Inc()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(cached)
			queriesTotal.WithLabelValues("hybrid", "success").Inc()
			queryLatency.WithLabelValues("hybrid", "success").Observe(time.Since(start).Seconds())
			return
		}
		cacheMisses.Inc()

		resp, err := searcher.SearchWithFusion(r.Context(), q, limit, alpha, fusionMethod)
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			queriesTotal.WithLabelValues("hybrid", "error").Inc()
			queryLatency.WithLabelValues("hybrid", "error").Observe(time.Since(start).Seconds())
			return
		}

		data, _ := json.Marshal(resp)
		setCache(r.Context(), rdb, key, data)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(data)

		queriesTotal.WithLabelValues("hybrid", "success").Inc()
		queryLatency.WithLabelValues("hybrid", "success").Observe(time.Since(start).Seconds())

		log.Printf("HYBRID '%s' → %d hits in %v", q, len(resp.Hits), time.Since(start))
	}
}

func shardsHandler(etcdEps string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cli, err := clientv3.New(clientv3.Config{
			Endpoints:   strings.Split(etcdEps, ","),
			DialTimeout: 3 * time.Second,
		})
		if err != nil {
			writeError(w, "etcd connect: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer cli.Close()

		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		resp, err := cli.Get(ctx, "/shards/active/", clientv3.WithPrefix())
		if err != nil {
			writeError(w, "etcd get: "+err.Error(), http.StatusInternalServerError)
			return
		}

		type shardInfo struct {
			ID      string `json:"id"`
			Address string `json:"address"`
		}

		shards := make([]shardInfo, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			key := string(kv.Key)
			parts := strings.Split(key, "/")
			id := parts[len(parts)-1]
			shards = append(shards, shardInfo{ID: id, Address: string(kv.Value)})
		}

		writeJSON(w, map[string]interface{}{
			"count":  len(shards),
			"shards": shards,
		})
	}
}

func hotTermsHandler(etcdEps string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cli, err := clientv3.New(clientv3.Config{
			Endpoints:   strings.Split(etcdEps, ","),
			DialTimeout: 3 * time.Second,
		})
		if err != nil {
			writeError(w, "etcd connect: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer cli.Close()

		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		resp, err := cli.Get(ctx, "/hot_terms/", clientv3.WithPrefix())
		if err != nil {
			writeError(w, "etcd get: "+err.Error(), http.StatusInternalServerError)
			return
		}

		type hotTerm struct {
			Term   string `json:"term"`
			Shards string `json:"shards"`
		}

		terms := make([]hotTerm, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			key := string(kv.Key)
			parts := strings.Split(key, "/")
			if len(parts) >= 3 {
				term := parts[2]
				terms = append(terms, hotTerm{Term: term, Shards: string(kv.Value)})
			}
		}

		writeJSON(w, map[string]interface{}{
			"count": len(terms),
			"terms": terms,
		})
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
