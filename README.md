# Distributed Search System

A production-grade distributed search engine built from scratch in Go, handling 24k+ Wikipedia documents across 8 shards with intelligent query routing and sub-millisecond cache hits.

## 🎯 Key Features

- **Distributed Architecture**: 8-shard horizontal scaling with etcd-based service discovery
- **Smart Query Routing**: Hot-term affinity routing (80/20 optimization for Zipf traffic)
- **Redis Cache Layer**: 5-minute TTL with thundering herd protection
- **Fault Tolerant**: 3-node etcd cluster, graceful shard failures
- **Production Metrics**: Prometheus-compatible metrics on all services
- **BM25 Relevance**: Bleve full-text search with highlighting

## 🏗️ Architecture

```
┌─────────────┐
│   Client    │
└──────┬──────┘
       │
       ▼
┌──────────────────────────────────────┐
│  Coordinator (Port 8090)             │
│  - Query routing (hot/cold)          │
│  - Cache-aside pattern (Redis)       │
│  - Fan-out/merge coordination        │
└─────┬────────────────────────────────┘
      │
      ├─► Redis Cache (256MB LRU) ──► 0.5ms hit
      │
      ├─► etcd Cluster (3 nodes)
      │   - /shards/active/*  → shard registry
      │   - /hot_terms/*      → routing hints
      │
      └─► 8x Shard Services (8081-8088)
          ├─► shard-0: 3.2k docs (search.bleve-0)
          ├─► shard-1: 3.1k docs (search.bleve-1)
          ├─► ...
          └─► shard-7: 3.1k docs (search.bleve-7)
```

## 🚀 Quick Start

### Prerequisites
- Docker & Docker Compose
- Go 1.21+ (for local development)

### One-Command Deploy

```bash
# Clone repo
git clone <your-repo>
cd distributed-search

# Ensure indexes are built (if not already)
# This creates search.bleve-0 through search.bleve-7
for i in {0..7}; do
  go run cmd/indexer/main.go \
    -input=shard-$i.jsonl \
    -index=search.bleve \
    -shard-id=$i \
    -batch-size=500
done

# Start everything
chmod +x start.sh
./start.sh
```

### Test It

```bash
# Search query (should hit cache or route to shards)
curl 'http://localhost:8090/search?q=distributed&limit=5' | jq

# Expected output:
{
  "query": "distributed",
  "shards": 8,
  "total_hits": 47,
  "routing_type": "hot",  # "hot" = 2 shards, "cold" = all 8
  "hits": [
    {
      "id": "wiki_1234",
      "score": 12.456,
      "title": "Distributed computing",
      "shard": "shard-0:8080"
    }
  ],
  "took": "3.2ms"
}

# List active shards
curl 'http://localhost:8090/shards' | jq

# View hot-term configuration
curl 'http://localhost:8090/hot-terms' | jq
```

## 📊 Performance Characteristics

| Query Type | Shards Hit | Latency | Notes |
|------------|-----------|---------|-------|
| **Hot + Cached** | 0 (cache) | 0.5ms | 80% of traffic |
| **Hot + Miss** | 2 shards | 3ms | Affinity routing |
| **Cold + Cached** | 0 (cache) | 0.5ms | Repeat queries |
| **Cold + Miss** | 8 shards | 18ms | Full fan-out |

**Throughput**: 1000 QPS sustained on M4 MacBook (single coordinator)

## 🧪 Development

### Local Development (without Docker)

```bash
# Terminal 1: Start etcd
docker compose up etcd0 etcd1 etcd2 -d

# Terminal 2: Start Redis
redis-server --port 6379 --maxmemory 256mb --maxmemory-policy allkeys-lru

# Terminal 3-10: Start 8 shards
for i in {0..7}; do
  port=$((8081 + i))
  go run cmd/searcher/main.go \
    -shard-id=$i \
    -port=$port \
    -index=search.bleve &
done

# Terminal 11: Start coordinator
go run cmd/coordinator/main.go

# Test
curl 'localhost:8090/search?q=go&limit=5'
```

### Build Data Pipeline

```bash
# 1. Download Wikipedia dump
wget https://dumps.wikimedia.org/enwiki/latest/enwiki-latest-pages-articles1.xml-p1p41242.bz2
bunzip2 enwiki-latest-pages-articles1.xml-p1p41242.bz2
mv enwiki-latest-pages-articles1.xml-p1p41242 data.xml

# 2. Ingest with sharding (MD5 hash-based)
go run cmd/ingester/main.go \
  -input=data.xml \
  -num-shards=8

# Output: shard-0.jsonl through shard-7.jsonl

# 3. Index each shard
for i in {0..7}; do
  go run cmd/indexer/main.go \
    -input=shard-$i.jsonl \
    -index=search.bleve \
    -shard-id=$i \
    -batch-size=500
done

# Output: search.bleve-0 through search.bleve-7 (27-57MB each)
```

## 🔥 Hot-Term Configuration

Configure popular queries to only hit subset of shards:

```bash
# Add hot terms via etcd
docker exec etcd0 etcdctl put /hot_terms/distributed/shards "0,1"
docker exec etcd0 etcdctl put /hot_terms/python/shards "2,3"
docker exec etcd0 etcdctl put /hot_terms/go/shards "0,2"

# Verify
docker exec etcd0 etcdctl get --prefix /hot_terms/

# Test routing
curl 'localhost:8090/search?q=distributed' | jq .routing_type
# Output: "hot" (only 2 shards queried)

curl 'localhost:8090/search?q=rust' | jq .routing_type
# Output: "cold" (all 8 shards queried)
```

## 📁 Project Structure

```
distributed-search/
├── cmd/
│   ├── ingester/main.go       # Streaming XML parser → shard-N.jsonl
│   ├── indexer/main.go        # Bleve indexer (shard-aware)
│   ├── searcher/main.go       # Shard service (etcd registration)
│   └── coordinator/main.go    # Query router (hot-term aware)
├── internal/
│   ├── index/indexer.go       # Bleve wrapper
│   ├── model/doc.go           # Document struct
│   └── search/search.go       # Result formatting
├── docker/
│   ├── Dockerfile.shard
│   └── Dockerfile.coordinator
├── docker-compose.yml         # Full stack orchestration
├── start.sh                   # One-command startup
└── README.md
```

## 🛠️ Technology Stack

- **Language**: Go 1.21
- **Search Engine**: [Bleve](https://github.com/blevesearch/bleve) (BM25 scoring)
- **Service Discovery**: etcd v3.5
- **Cache**: Redis 7 (LRU eviction)
- **Metrics**: Prometheus client
- **HTTP Router**: Gorilla Mux
- **Containerization**: Docker + Docker Compose

## 🎓 Learning Resources

This project demonstrates:

1. **Distributed Systems**: Sharding, service discovery, fault tolerance
2. **Search Engineering**: Inverted indexes, BM25 relevance, query routing
3. **Performance Optimization**: Caching strategies, hotspot mitigation
4. **Production Readiness**: Health checks, graceful shutdown, observability

## 📈 Scaling Beyond

**To 1M documents**:
- Download larger Wikipedia dump
- Increase shards (16-32)
- Same code works unchanged

**To 1M QPS**:
- Add more coordinator replicas (stateless)
- Redis Cluster for cache
- Load balancer (nginx/HAProxy)

**Geographic Distribution**:
- Multi-region etcd clusters
- Regional shard replicas
- CDN for static assets

## 🐛 Troubleshooting

**Shards not registering with etcd?**
```bash
# Check etcd is healthy
docker exec etcd0 etcdctl endpoint health

# Check shard logs
docker compose logs shard-0
```

**Cache not working?**
```bash
# Test Redis connection
docker exec redis redis-cli ping

# Check coordinator logs for cache hits/misses
docker compose logs coordinator | grep CACHE
```

**Slow queries?**
```bash
# Check which shards were queried
curl 'localhost:8090/search?q=test' | jq .routing_type

# Monitor Prometheus metrics
curl 'localhost:8081/metrics' | grep search_query_latency
```

## 📝 License

MIT

## 🙏 Acknowledgments

Built as a learning project to understand distributed search systems like Elasticsearch, PlanetScale's Vitess architecture, and production caching patterns.

---

**Author**: Devanshu Sharma
**Year**: 2026  
**Stack**: Go • Bleve • etcd • Redis • Docker