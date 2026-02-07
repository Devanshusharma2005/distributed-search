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
	etcdEps   = flag.String("etcd", "localhost:2379,localhost:2381,localhost:2383", "etcd endpoints")
	port      = flag.Int("port", 8090, "coordinator HTTP port")
	limit     = flag.Int("limit", 20, "default global top-K limit")
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

type ShardHit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Title string  `json:"title"`
	Shard string  `json:"shard,omitempty"`
}

type CoordinatorResponse struct {
	Query      string     `json:"query"`
	Shards     int        `json:"shards"`
	TotalHits  int        `json:"total_hits"`
	Hits       []ShardHit `json:"hits"`
	Took       string     `json:"took"`
	RoutingType string    `json:"routing_type"` // "hot" or "cold"
}

func main() {
	flag.Parse()

	rdb = redis.NewClient(&redis.Options{
		Addr: *redisAddr,
	})

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("⚠️  Redis not available at %s: %v (cache disabled)", *redisAddr, err)
		rdb = nil
	} else {
		log.Printf("✅ Redis connected at %s", *redisAddr)
	}

	log.Printf("🚀 Coordinator starting on port %d (Phase 4: Hot-term routing)...", *port)

	r := mux.NewRouter()
	r.HandleFunc("/search", coordSearchHandler).Methods("GET")
	r.HandleFunc("/shards", listShardsHandler).Methods("GET")
	r.HandleFunc("/hot-terms", listHotTermsHandler).Methods("GET")
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}).Methods("GET")

	log.Printf("🌐 Coordinator ready at :%d (cache=%v, hot-routing=enabled)", *port, rdb != nil)
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(*port), r))
}

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

	cacheKey := fmt.Sprintf("search:%s:%d", q, qLimit)

	if rdb != nil {
		cached, err := rdb.Get(ctx, cacheKey).Result()
		if err == nil {
			log.Printf("✅ CACHE HIT: %s", cacheKey)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("X-Took", time.Since(start).String())
			w.Write([]byte(cached))
			return
		}

		lockKey := cacheKey + ":lock"
		locked, _ := rdb.SetNX(ctx, lockKey, "1", 2*time.Second).Result()
		if locked {
			defer rdb.Del(ctx, lockKey)
			log.Printf("🔄 CACHE MISS + LOCK: %s", cacheKey)
		} else {
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
		}
	}

	allShards, err := discoverShards()
	if err != nil || len(allShards) == 0 {
		http.Error(w, fmt.Sprintf(`{"error": "no shards available: %v"}`, err), http.StatusServiceUnavailable)
		return
	}

	perShardLimit := qLimit * 3
	var shardHits []ShardHit
	var routingType string

	hotShardIDs, isHot := getHotTermShards(q)
	if isHot && len(hotShardIDs) > 0 {
		hotShardAddrs := make([]string, 0, len(hotShardIDs))
		for _, shardID := range hotShardIDs {
			if shardID >= 0 && shardID < len(allShards) {
				hotShardAddrs = append(hotShardAddrs, allShards[shardID])
			}
		}
		log.Printf("🔥 HOT TERM '%s' → %d affinity shards (not %d)", q, len(hotShardAddrs), len(allShards))
		shardHits = fanoutQueryParallel(hotShardAddrs, q, perShardLimit)
		routingType = "hot"
		updateHotTermStats(q, len(shardHits), len(hotShardAddrs))
	} else {
		log.Printf("❄️  COLD TERM '%s' → ALL %d shards", q, len(allShards))
		shardHits = fanoutQueryParallel(allShards, q, perShardLimit)
		routingType = "cold"
	}

	topHits := mergeTopK(shardHits, qLimit)

	resp := CoordinatorResponse{
		Query:       q,
		Shards:      len(allShards),
		TotalHits:   len(shardHits),
		Hits:        topHits,
		Took:        time.Since(start).String(),
		RoutingType: routingType,
	}

	resultJSON, _ := json.Marshal(resp)

	if rdb != nil {
		rdb.SetEx(ctx, cacheKey, resultJSON, cacheTTL)
		log.Printf("✅ BACKEND + CACHED: %s (%v, routing=%s)", cacheKey, time.Since(start), routingType)
	}

	w.Header().Set("Content-Type", "application/json")
	if rdb != nil {
		w.Header().Set("X-Cache", "MISS")
	}
	w.Header().Set("X-Took", time.Since(start).String())
	w.Header().Set("X-Routing", routingType)
	w.Write(resultJSON)

	log.Printf("✅ Coordinator: '%s' → %d hits (%s routing) in %v", q, len(topHits), routingType, time.Since(start))
}


func getHotTermShards(term string) ([]int, bool) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(*etcdEps, ","),
		DialTimeout: 1 * time.Second,
	})
	if err != nil {
		return nil, false
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	key := fmt.Sprintf("/hot_terms/%s/shards", term)
	resp, err := cli.Get(ctx, key)
	if err != nil || len(resp.Kvs) == 0 {
		return nil, false // Not a hot term
	}

	// IDs: "0,1,2"
	shardStr := string(resp.Kvs[0].Value)
	idStrs := strings.Split(shardStr, ",")
	shardIDs := make([]int, 0, len(idStrs))

	for _, idStr := range idStrs {
		idStr = strings.TrimSpace(idStr)
		if id, err := strconv.Atoi(idStr); err == nil {
			shardIDs = append(shardIDs, id)
		}
	}

	return shardIDs, true
}

func updateHotTermStats(term string, hitCount, shardCount int) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(*etcdEps, ","),
		DialTimeout: 1 * time.Second,
	})
	if err != nil {
		return
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	key := fmt.Sprintf("/hot_terms/%s/stats", term)
	value := fmt.Sprintf("hits:%d shards:%d ts:%d", hitCount, shardCount, time.Now().Unix())
	cli.Put(ctx, key, value)
}

// discoverShards queries etcd for /shards/active/*
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

// fanoutQueryParallel sends query to specified shards in parallel
func fanoutQueryParallel(shards []string, q string, perShardLimit int) []ShardHit {
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

	go func() {
		wg.Wait()
		close(hitsCh)
	}()

	allHits := make([]ShardHit, 0, len(shards)*perShardLimit)
	for hit := range hitsCh {
		allHits = append(allHits, hit)
	}

	return allHits
}

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

func getString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

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

func listHotTermsHandler(w http.ResponseWriter, r *http.Request) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(*etcdEps, ","),
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%v"}`, err), http.StatusInternalServerError)
		return
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := cli.Get(ctx, "/hot_terms/", clientv3.WithPrefix())
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%v"}`, err), http.StatusInternalServerError)
		return
	}

	hotTerms := make(map[string]map[string]string)
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		value := string(kv.Value)

		parts := strings.Split(strings.TrimPrefix(key, "/hot_terms/"), "/")
		if len(parts) == 2 {
			term := parts[0]
			field := parts[1]

			if _, exists := hotTerms[term]; !exists {
				hotTerms[term] = make(map[string]string)
			}
			hotTerms[term][field] = value
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"hot_terms": hotTerms,
		"count":     len(hotTerms),
	})
}