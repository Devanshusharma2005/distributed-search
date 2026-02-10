package hybrid

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type EmbeddingClient interface {
	GetEmbedding(ctx context.Context, text string) ([]float64, error)
}

type ShardHit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Title string  `json:"title"`
	Shard string  `json:"shard,omitempty"`
}

type HybridResult struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	KeywordScore   float64 `json:"keyword_score"`
	SemanticScore  float64 `json:"semantic_score,omitempty"`
	HybridScore    float64 `json:"hybrid_score"`
	Shard          string  `json:"shard"`
}

type HybridResponse struct {
	Query          string         `json:"query"`
	QueryVector    []float64      `json:"query_vector,omitempty"`
	KeywordHits    int            `json:"keyword_hits"`
	SemanticTopK   int            `json:"semantic_topk"`
	FusionAlpha    float64        `json:"fusion_alpha"`
	Hits           []HybridResult `json:"hits"`
	Took           string         `json:"took"`
	RoutingType    string         `json:"routing_type"`
}

type HybridSearcher struct {
	embedClient  EmbeddingClient
	etcdEps      string
	defaultAlpha float64
}

func NewHybridSearcher(embedClient EmbeddingClient, etcdEps string) *HybridSearcher {
	return &HybridSearcher{
		embedClient:  embedClient,
		etcdEps:      etcdEps,
		defaultAlpha: 0.7, // 70% keyword, 30% semantic
	}
}

func (h *HybridSearcher) Search(ctx context.Context, query string, limit int, alpha float64) (*HybridResponse, error) {
	start := time.Now()

	if alpha == 0 {
		alpha = h.defaultAlpha
	}

	embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	queryVector, err := h.embedClient.GetEmbedding(embedCtx, query)
	if err != nil {
		log.Printf("⚠️  Embedding failed for '%s': %v (falling back to keyword-only)", query, err)
		queryVector = nil 
	}

	allShards, err := h.discoverShards()
	if err != nil {
		return nil, fmt.Errorf("discover shards: %w", err)
	}

	hotShardIDs, isHot := h.getHotTermShards(query)
	
	var shardHits []ShardHit
	var routingType string
	var targetShards []string

	if isHot && len(hotShardIDs) > 0 {
		hotShardAddrs := make([]string, 0, len(hotShardIDs))
		for _, shardID := range hotShardIDs {
			if shardID >= 0 && shardID < len(allShards) {
				hotShardAddrs = append(hotShardAddrs, allShards[shardID])
			}
		}
		log.Printf("🔥 HYBRID: HOT TERM '%s' → %d shards", query, len(hotShardAddrs))
		targetShards = hotShardAddrs
		routingType = "hot"
	} else {
		log.Printf("❄️  HYBRID: COLD TERM '%s' → ALL %d shards", query, len(allShards))
		targetShards = allShards
		routingType = "cold"
	}

	shardHits = h.fanoutQueryParallel(targetShards, query, 100)

	hybridResults := make([]HybridResult, len(shardHits))
	
	for i, hit := range shardHits {
		hybridResults[i] = HybridResult{
			ID:           hit.ID,
			Title:        hit.Title,
			KeywordScore: hit.Score,
			Shard:        hit.Shard,
		}

		if queryVector != nil {
			semanticScore := 0.0 
			
			hybridResults[i].SemanticScore = semanticScore
			hybridResults[i].HybridScore = alpha*hit.Score + (1-alpha)*semanticScore
		} else {
			hybridResults[i].HybridScore = hit.Score
		}
	}

	sort.Slice(hybridResults, func(i, j int) bool {
		return hybridResults[i].HybridScore > hybridResults[j].HybridScore
	})

	if len(hybridResults) > limit {
		hybridResults = hybridResults[:limit]
	}

	resp := &HybridResponse{
		Query:        query,
		QueryVector:  queryVector,
		KeywordHits:  len(shardHits),
		SemanticTopK: len(hybridResults),
		FusionAlpha:  alpha,
		Hits:         hybridResults,
		Took:         time.Since(start).String(),
		RoutingType:  routingType,
	}

	log.Printf("✅ HYBRID: '%s' → %d candidates, %d final (alpha=%.2f) in %v", 
		query, len(shardHits), len(hybridResults), alpha, time.Since(start))

	return resp, nil
}

func (h *HybridSearcher) discoverShards() ([]string, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(h.etcdEps, ","),
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

func (h *HybridSearcher) getHotTermShards(term string) ([]int, bool) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(h.etcdEps, ","),
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
		return nil, false
	}

	// Parse comma-separated shard IDs: "0,1,2"
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

func (h *HybridSearcher) fanoutQueryParallel(shards []string, q string, perShardLimit int) []ShardHit {
	var wg sync.WaitGroup
	hitsCh := make(chan ShardHit, len(shards)*perShardLimit)

	for _, shardAddr := range shards {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			hits := h.queryShard(addr, q, perShardLimit)
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

func (h *HybridSearcher) queryShard(shardAddr, q string, limit int) []ShardHit {
	queryURL := fmt.Sprintf("http://%s/search?q=%s&limit=%d",
		shardAddr, url.QueryEscape(q), limit)

	resp, err := http.Get(queryURL)
	if err != nil {
		log.Printf("Shard %s error: %v", shardAddr, err)
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
		log.Printf("Shard %s decode error: %v", shardAddr, err)
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

func getString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}

	var dotProduct, normA, normB float64
	for i := 0; i < len(a); i++ {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}