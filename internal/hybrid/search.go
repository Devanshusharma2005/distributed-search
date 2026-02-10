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
	ID          string    `json:"id"`
	Score       float64   `json:"score"`
	Title       string    `json:"title"`
	TitleVector []float64 `json:"title_vector,omitempty"`
	Shard       string    `json:"shard,omitempty"`
}

type HybridResult struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	KeywordScore  float64 `json:"keyword_score"`
	SemanticScore float64 `json:"semantic_score,omitempty"`
	HybridScore   float64 `json:"hybrid_score"`
	Shard         string  `json:"shard"`
	FusionMethod  string  `json:"fusion_method,omitempty"` 
}

type HybridResponse struct {
	Query        string         `json:"query"`
	QueryVector  []float64      `json:"query_vector,omitempty"`
	KeywordHits  int            `json:"keyword_hits"`
	SemanticTopK int            `json:"semantic_topk"`
	FusionAlpha  float64        `json:"fusion_alpha,omitempty"`   
	FusionMethod string         `json:"fusion_method"`            
	FusionK      int            `json:"fusion_k,omitempty"`       
	Hits         []HybridResult `json:"hits"`
	Took         string         `json:"took"`
	RoutingType  string         `json:"routing_type"`
}

type HybridSearcher struct {
	embedClient  EmbeddingClient
	etcdEps      string
	defaultAlpha float64
	rrfK         int 
}

func NewHybridSearcher(embedClient EmbeddingClient, etcdEps string) *HybridSearcher {
	return &HybridSearcher{
		embedClient:  embedClient,
		etcdEps:      etcdEps,
		defaultAlpha: 0.7, 
		rrfK:         60,  
	}
}

func (h *HybridSearcher) Search(ctx context.Context, query string, limit int, alpha float64) (*HybridResponse, error) {
	return h.SearchWithFusion(ctx, query, limit, alpha, "rrf")
}

func (h *HybridSearcher) SearchWithFusion(ctx context.Context, query string, limit int, alpha float64, fusionMethod string) (*HybridResponse, error) {
	start := time.Now()

	if alpha == 0 {
		alpha = h.defaultAlpha
	}

	embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	queryVector, err := h.embedClient.GetEmbedding(embedCtx, query)
	if err != nil {
		log.Printf("Embedding failed for '%s': %v (falling back to keyword-only)", query, err)
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
		log.Printf("HYBRID: HOT TERM '%s' → %d shards", query, len(hotShardAddrs))
		targetShards = hotShardAddrs
		routingType = "hot"
	} else {
		log.Printf("HYBRID: COLD TERM '%s' → ALL %d shards", query, len(allShards))
		targetShards = allShards
		routingType = "cold"
	}

	retrievalLimit := 100
	if fusionMethod != "rrf" {
		retrievalLimit = limit * 3 
	}

	shardHits = h.fanoutQueryParallel(targetShards, query, retrievalLimit)

	var hybridResults []HybridResult
	var semanticCount int

	if fusionMethod == "rrf" && queryVector != nil {
		hybridResults, semanticCount = h.fuseWithRRF(shardHits, queryVector, limit)
	} else {
		hybridResults, semanticCount = h.fuseWithWeights(shardHits, queryVector, alpha, limit)
	}

	resp := &HybridResponse{
		Query:        query,
		QueryVector:  queryVector,
		KeywordHits:  len(shardHits),
		SemanticTopK: len(hybridResults),
		FusionMethod: fusionMethod,
		Hits:         hybridResults,
		Took:         time.Since(start).String(),
		RoutingType:  routingType,
	}

	if fusionMethod == "rrf" {
		resp.FusionK = h.rrfK
	} else {
		resp.FusionAlpha = alpha
	}

	log.Printf("HYBRID [%s]: '%s' → %d candidates, %d with vectors, %d final in %v",
		strings.ToUpper(fusionMethod), query, len(shardHits), semanticCount, len(hybridResults), time.Since(start))

	return resp, nil
}

func (h *HybridSearcher) fuseWithRRF(hits []ShardHit, queryVector []float64, limit int) ([]HybridResult, int) {
	bm25Ranked := make([]ShardHit, len(hits))
	copy(bm25Ranked, hits)
	sort.Slice(bm25Ranked, func(i, j int) bool {
		return bm25Ranked[i].Score > bm25Ranked[j].Score
	})

	type ScoredDoc struct {
		Hit           ShardHit
		SemanticScore float64
	}

	scoredDocs := make([]ScoredDoc, 0, len(hits))
	semanticCount := 0

	for _, hit := range hits {
		if queryVector != nil && hit.TitleVector != nil && len(hit.TitleVector) > 0 {
			semanticScore := CosineSimilarity(queryVector, hit.TitleVector)
			scoredDocs = append(scoredDocs, ScoredDoc{
				Hit:           hit,
				SemanticScore: semanticScore,
			})
			semanticCount++
		} else {
			scoredDocs = append(scoredDocs, ScoredDoc{
				Hit:           hit,
				SemanticScore: 0.0,
			})
		}
	}

	semanticRanked := make([]ScoredDoc, len(scoredDocs))
	copy(semanticRanked, scoredDocs)
	sort.Slice(semanticRanked, func(i, j int) bool {
		return semanticRanked[i].SemanticScore > semanticRanked[j].SemanticScore
	})

	bm25Ranks := make(map[string]int)
	for rank, hit := range bm25Ranked {
		bm25Ranks[hit.ID] = rank + 1 
	}

	semanticRanks := make(map[string]int)
	for rank, doc := range semanticRanked {
		semanticRanks[doc.Hit.ID] = rank + 1
	}

	type RRFResult struct {
		ID            string
		Title         string
		KeywordScore  float64
		SemanticScore float64
		RRFScore      float64
		Shard         string
	}

	rrfResults := make(map[string]*RRFResult)

	for id := range bm25Ranks {
		if _, exists := rrfResults[id]; !exists {
			rrfResults[id] = &RRFResult{ID: id}
		}
	}

	for id := range semanticRanks {
		if _, exists := rrfResults[id]; !exists {
			rrfResults[id] = &RRFResult{ID: id}
		}
	}

	k := float64(h.rrfK)

	for id, result := range rrfResults {
		bm25Rank := bm25Ranks[id]
		if bm25Rank == 0 {
			bm25Rank = 101
		}

		semanticRank := semanticRanks[id]
		if semanticRank == 0 {
			semanticRank = 101 
		}

		result.RRFScore = 1.0/(k+float64(bm25Rank)) + 1.0/(k+float64(semanticRank))

		for _, hit := range hits {
			if hit.ID == id {
				result.Title = hit.Title
				result.KeywordScore = hit.Score
				result.Shard = hit.Shard
				break
			}
		}

		for _, doc := range scoredDocs {
			if doc.Hit.ID == id {
				result.SemanticScore = doc.SemanticScore
				break
			}
		}
	}

	rrfList := make([]RRFResult, 0, len(rrfResults))
	for _, r := range rrfResults {
		rrfList = append(rrfList, *r)
	}

	sort.Slice(rrfList, func(i, j int) bool {
		return rrfList[i].RRFScore > rrfList[j].RRFScore
	})

	if len(rrfList) > limit {
		rrfList = rrfList[:limit]
	}

	finalResults := make([]HybridResult, len(rrfList))
	for i, r := range rrfList {
		finalResults[i] = HybridResult{
			ID:            r.ID,
			Title:         r.Title,
			KeywordScore:  r.KeywordScore,
			SemanticScore: r.SemanticScore,
			HybridScore:   r.RRFScore,
			Shard:         r.Shard,
			FusionMethod:  "RRF",
		}
	}

	return finalResults, semanticCount
}

func (h *HybridSearcher) fuseWithWeights(hits []ShardHit, queryVector []float64, alpha float64, limit int) ([]HybridResult, int) {
	hybridResults := make([]HybridResult, len(hits))
	semanticCount := 0

	for i, hit := range hits {
		hybridResults[i] = HybridResult{
			ID:           hit.ID,
			Title:        hit.Title,
			KeywordScore: hit.Score,
			Shard:        hit.Shard,
			FusionMethod: "weighted",
		}

		if queryVector != nil && hit.TitleVector != nil && len(hit.TitleVector) > 0 {
			semanticScore := CosineSimilarity(queryVector, hit.TitleVector)
			hybridResults[i].SemanticScore = semanticScore
			hybridResults[i].HybridScore = alpha*hit.Score + (1-alpha)*semanticScore
			semanticCount++
		} else {
			hybridResults[i].SemanticScore = 0.0
			hybridResults[i].HybridScore = hit.Score
		}
	}

	sort.Slice(hybridResults, func(i, j int) bool {
		return hybridResults[i].HybridScore > hybridResults[j].HybridScore
	})

	if len(hybridResults) > limit {
		hybridResults = hybridResults[:limit]
	}

	return hybridResults, semanticCount
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
		log.Printf("Shard %s returned status %d", shardAddr, resp.StatusCode)
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

		if vecField, ok := h.Fields["title_vector"]; ok {
			hits[i].TitleVector = parseVector(vecField)
		}
	}

	return hits
}

func parseVector(field interface{}) []float64 {
	switch v := field.(type) {
	case []interface{}:
		vec := make([]float64, len(v))
		for i, val := range v {
			if f, ok := val.(float64); ok {
				vec[i] = f
			}
		}
		return vec
	case []float64:
		return v
	default:
		return nil
	}
}

func getString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
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