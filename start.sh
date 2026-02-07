#!/bin/bash
set -e

echo "🚀 Starting Distributed Search System..."
echo ""
echo "Components:"
echo "  - 3-node etcd cluster (service discovery)"
echo "  - Redis cache (256MB LRU)"
echo "  - 8 shard services (24k docs total)"
echo "  - Coordinator with hot-term routing"
echo ""

# Check if indexes exist
if [ ! -d "search.bleve-0" ]; then
    echo "❌ ERROR: Shard indexes not found!"
    echo ""
    echo "Please run the indexer first:"
    echo "  for i in {0..7}; do"
    echo "    go run cmd/indexer/main.go -input=shard-\$i.jsonl -index=search.bleve -shard-id=\$i -batch-size=500"
    echo "  done"
    echo ""
    exit 1
fi

# Start services
echo "📦 Building Docker images..."
docker compose build

echo ""
echo "🔄 Starting services..."
docker compose up -d

echo ""
echo "⏳ Waiting for services to be healthy..."
sleep 15

# Check etcd
echo "  ✓ etcd cluster..."
docker exec etcd0 etcdctl endpoint health --endpoints=http://etcd0:2379,http://etcd1:2379,http://etcd2:2379

# Check Redis
echo "  ✓ Redis cache..."
docker exec redis redis-cli ping

# Check coordinator
echo "  ✓ Coordinator..."
curl -s http://localhost:8090/health || echo "  ⚠️  Coordinator not ready yet"

echo ""
echo "✅ System is running!"
echo ""
echo "📡 Access Points:"
echo "  Coordinator:  http://localhost:8090"
echo "  etcd:         http://localhost:2379"
echo "  Redis:        localhost:6379"
echo ""
echo "🧪 Test Commands:"
echo "  # Search query"
echo "  curl 'http://localhost:8090/search?q=distributed&limit=5' | jq"
echo ""
echo "  # List active shards"
echo "  curl 'http://localhost:8090/shards' | jq"
echo ""
echo "  # List hot terms"
echo "  curl 'http://localhost:8090/hot-terms' | jq"
echo ""
echo "  # View logs"
echo "  docker compose logs -f coordinator"
echo "  docker compose logs -f shard-0"
echo ""
echo "  # Stop system"
echo "  docker compose down"
echo ""