package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

var (
	etcdEps  = flag.String("etcd", "localhost:2379,localhost:2381,localhost:2383", "etcd endpoints")
	port     = flag.Int("port", 8090, "coordinator HTTP port")
	limit    = flag.Int("limit", 20, "default global top-K limit")
	redisAddr = flag.String("redis", "localhost:6379", "redis address")
	cacheTTL  = 5 * time.Minute
)

var (
	ctx context.Context
	rdb *redis.Client
)

func init() {
	ctx = context.Background()
}

// ShardHit represents a single result from a shard
type ShardHit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Title string  `json:"title"`
	Shard string  `json:"shard,omitempty"`
}

// CoordinatorResponse is the merged response sent to the client
type CoordinatorResponse struct {
	Query     string     `json:"query"`
	Shards    int        `json:"shards"`
	TotalHits int        `json:"total_hits"`
	Hits      []ShardHit `json:"hits"`
	Took      string     `json:"took"`
}

func main() {
	flag.Parse()

	// Initialize Redis client
	rdb = redis.NewClient(&redis.Options{
		Addr: *redisAddr,
	})

	// Test Redis connection
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("⚠️  Redis not available at %s: %v (cache disabled)", *redisAddr, err)
		rdb = nil
	} else {
		log.Printf("✅ Redis connected at %s", *redisAddr)
	}

	log.Printf("🚀 Coordinator starting on port %d...", *port)

	r := mux.NewRouter()
	r.HandleFunc("/search", coordSearchHandler).Methods("GET")
	r.HandleFunc("/shards", listShardsHandler).Methods("GET")
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}).Methods("GET")

	log.Printf("🌐 Coordinator ready at :%d (cache=%v)", *port, rdb != nil)
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(*port), r))
}

// coordSearchHandler handles /search?q=... requests with Redis cache
func coordSearchHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, `{"error": "missing 'q' parameter"}`, http.StatusBadRequest)
		return
	}

	qLimit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if qLimit <= 0 {
		qLimit = *limit
	}

	// CACHE KEY: "search:<query>:<limit>"
	cacheKey := fmt.Sprintf("search:%s:%d", q, qLimit)

	// 1. CACHE LOOKUP (if Redis is available)
	if rdb != nil {
		cached, err := rdb.Get(ctx, cacheKey).Result()
		if err == nil {
			// CACHE HIT - return immediately (~1ms)
			log.Printf("✅ CACHE HIT: %s", cacheKey)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("X-Took", time.Since(start).String())
			w.Write([]byte(cached))
			return
		}

		// CACHE MISS - set lock to prevent thundering herd
		lockKey := cacheKey + ":lock"
		locked, _ := rdb.SetNX(ctx, lockKey, "1", 2*time.Second).Result()
		if locked {
			// This request won the race - populate cache
			defer rdb.Del(ctx, lockKey)
			log.Printf("🔄 CACHE MISS + LOCK: %s", cacheKey)
		} else {
			// Another request is populating - wait briefly and retry
			time.Sleep(50 * time.Millisecond)
			cached, _ := rdb.Get(ctx, cacheKey).Result()
			if cached != "" {
				log.Printf("✅ CACHE populated by other request: %s", cacheKey)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "HIT_WAIT")
				w.Header().Set("X-Took", time.Since(start).String())
				w.Write([]byte(cached))
				return
			}
			// If still not there, fall through to backend query
		}
	}

	// 2. BACKEND QUERY - discover shards from etcd
	shards, err := discoverShards()
	if err != nil || len(shards) == 0 {
		http.Error(w, fmt.Sprintf(`{"error": "no shards available: %v"}`, err), http.StatusServiceUnavailable)
		return
	}

	log.Printf("📡 Fan-out to %d shards for q='%s' limit=%d", len(shards), q, qLimit)

	// 3. Fan out queries to all shards in parallel
	perShardLimit := qLimit * 3 // overfetch for better global top-K
	shardHits := fanoutQuery(shards, q, perShardLimit)

	// 4. Global top-K merge
	topHits := mergeTopK(shardHits, qLimit)

	// 5. Build response
	resp := CoordinatorResponse{
		Query:     q,
		Shards:    len(shards),
		TotalHits: len(shardHits),
		Hits:      topHits,
		Took:      time.Since(start).String(),
	}

	resultJSON, _ := json.Marshal(resp)

	// 6. CACHE POPULATE (if Redis is available)
	if rdb != nil {
		rdb.SetEx(ctx, cacheKey, resultJSON, cacheTTL)
		log.Printf("✅ BACKEND + CACHED: %s (%v)", cacheKey, time.Since(start))
	}

	w.Header().Set("Content-Type", "application/json")
	if rdb != nil {
		w.Header().Set("X-Cache", "MISS")
	}
	w.Header().Set("X-Took", time.Since(start).String())
	w.Write(resultJSON)

	log.Printf("✅ Coordinator: '%s' → %d hits from %d shards in %v", q, len(topHits), len(shards), time.Since(start))
}

// discoverShards queries etcd for /shards/active/* and returns list of shard addresses
func discoverShards() ([]string, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(*etcdEps, ","),
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("etcd connect: %w", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Prefix scan for all active shards
	resp, err := cli.Get(ctx, "/shards/active/", clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd get: %w", err)
	}

	shards := make([]string, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		shardAddr := string(kv.Value)
		shards = append(shards, shardAddr)
	}

	return shards, nil
}

// fanoutQuery sends query to all shards in parallel and collects results
func fanoutQuery(shards []string, q string, perShardLimit int) []ShardHit {
	var wg sync.WaitGroup
	hitsCh := make(chan ShardHit, len(shards)*perShardLimit)

	for _, shardAddr := range shards {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			hits := queryShard(addr, q, perShardLimit)
			for _, hit := range hits {
				hitsCh <- hit
			}
		}(shardAddr)
	}

	// Close channel once all goroutines finish
	go func() {
		wg.Wait()
		close(hitsCh)
	}()

	// Collect all hits
	allHits := make([]ShardHit, 0, len(shards)*perShardLimit)
	for hit := range hitsCh {
		allHits = append(allHits, hit)
	}

	return allHits
}

// queryShard sends HTTP request to a single shard and parses Bleve response
func queryShard(shardAddr, q string, limit int) []ShardHit {
	queryURL := fmt.Sprintf("http://%s/search?q=%s&limit=%d",
		shardAddr, url.QueryEscape(q), limit)

	resp, err := http.Get(queryURL)
	if err != nil {
		log.Printf("⚠️  Shard %s error: %v", shardAddr, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("⚠️  Shard %s returned status %d", shardAddr, resp.StatusCode)
		return nil
	}

	var bleveRes struct {
		Hits []struct {
			ID     string                 `json:"id"`
			Score  float64                `json:"score"`
			Fields map[string]interface{} `json:"fields"`
		} `json:"hits"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&bleveRes); err != nil {
		log.Printf("⚠️  Shard %s decode error: %v", shardAddr, err)
		return nil
	}

	hits := make([]ShardHit, len(bleveRes.Hits))
	for i, h := range bleveRes.Hits {
		hits[i] = ShardHit{
			ID:    h.ID,
			Score: h.Score,
			Title: getString(h.Fields["title"]),
			Shard: shardAddr,
		}
	}

	return hits
}

// mergeTopK sorts all hits by score and returns top K
func mergeTopK(allHits []ShardHit, k int) []ShardHit {
	sort.Slice(allHits, func(i, j int) bool {
		return allHits[i].Score > allHits[j].Score
	})

	if len(allHits) <= k {
		return allHits
	}
	return allHits[:k]
}

// getString safely extracts string from interface{}
func getString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// listShardsHandler shows currently active shards
func listShardsHandler(w http.ResponseWriter, r *http.Request) {
	shards, err := discoverShards()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"shards": shards,
		"count":  len(shards),
	})
}