#!/bin/bash
set -e

echo "🚀 PHASE 6 - DAY 1: Rebuilding Indexes with Vector Embeddings"
echo "=============================================================="
echo ""

# Configuration
OLLAMA_URL="http://localhost:11434"
BATCH_SIZE=100
SKIP_VECTORS=false

# Parse arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    --skip-vectors)
      SKIP_VECTORS=true
      shift
      ;;
    --batch-size)
      BATCH_SIZE="$2"
      shift 2
      ;;
    *)
      echo "Unknown option: $1"
      echo "Usage: $0 [--skip-vectors] [--batch-size N]"
      exit 1
      ;;
  esac
done

# Step 1: Verify Ollama is running
echo "1️⃣  Checking Ollama availability..."
if ! curl -s "$OLLAMA_URL/api/tags" > /dev/null 2>&1; then
  echo "❌ Ollama not reachable at $OLLAMA_URL"
  echo "   Start it with: docker compose -f docker-compose.yml up -d ollama"
  exit 1
fi
echo "✅ Ollama is running"
echo ""

# Step 2: Verify all-minilm model is downloaded
echo "2️⃣  Checking all-minilm model..."
if ! docker exec ollama ollama list | grep -q "all-minilm"; then
  echo "⬇️  Downloading all-minilm model..."
  docker exec ollama ollama pull all-minilm
else
  echo "✅ all-minilm model ready"
fi
echo ""

# Step 3: Backup old indexes
echo "3️⃣  Backing up old indexes..."
BACKUP_DIR="search-backup-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"

for i in {0..7}; do
  if [ -d "search.bleve-$i" ]; then
    echo "   Backing up search.bleve-$i → $BACKUP_DIR/"
    cp -r "search.bleve-$i" "$BACKUP_DIR/"
  fi
done
echo "✅ Backup complete: $BACKUP_DIR/"
echo ""

# Step 4: Remove old indexes
echo "4️⃣  Removing old indexes..."
for i in {0..7}; do
  if [ -d "search.bleve-$i" ]; then
    rm -rf "search.bleve-$i"
    echo "   Removed search.bleve-$i"
  fi
done
echo "✅ Old indexes removed"
echo ""

# Step 5: Build indexer
echo "5️⃣  Building vector-enabled indexer..."
go build -o indexer-vector cmd/indexer/main.go
echo "✅ Indexer built: ./indexer-vector"
echo ""

# Step 6: Index all shards with embeddings
echo "6️⃣  Indexing shards with vector embeddings..."
echo "   (This will take ~5-10 minutes for 24k docs)"
echo ""

START_TIME=$(date +%s)

for i in {0..7}; do
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "   SHARD $i: Starting..."
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  
  SHARD_START=$(date +%s)
  
  if [ "$SKIP_VECTORS" = true ]; then
    ./indexer-vector \
      -input="shard-$i.jsonl" \
      -index="search.bleve" \
      -shard-id="$i" \
      -batch-size="$BATCH_SIZE" \
      -ollama="$OLLAMA_URL" \
      -skip-vectors
  else
    ./indexer-vector \
      -input="shard-$i.jsonl" \
      -index="search.bleve" \
      -shard-id="$i" \
      -batch-size="$BATCH_SIZE" \
      -ollama="$OLLAMA_URL"
  fi
  
  SHARD_END=$(date +%s)
  SHARD_ELAPSED=$((SHARD_END - SHARD_START))
  
  # Check index was created
  if [ -d "search.bleve-$i" ]; then
    SIZE=$(du -sh "search.bleve-$i" | cut -f1)
    echo "✅ Shard $i complete: $SIZE (${SHARD_ELAPSED}s)"
  else
    echo "❌ Shard $i FAILED: index not created"
    exit 1
  fi
  echo ""
done

END_TIME=$(date +%s)
TOTAL_ELAPSED=$((END_TIME - START_TIME))
MINUTES=$((TOTAL_ELAPSED / 60))
SECONDS=$((TOTAL_ELAPSED % 60))

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ ALL SHARDS INDEXED"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Step 7: Summary
echo "📊 SUMMARY:"
echo "   Total time: ${MINUTES}m ${SECONDS}s"
echo "   Shards: 8"
echo "   Documents: ~24,765"
if [ "$SKIP_VECTORS" = false ]; then
  echo "   Embeddings: ~24,765 × 384 dims"
fi
echo ""

# Show index sizes
echo "📁 Index Sizes:"
for i in {0..7}; do
  if [ -d "search.bleve-$i" ]; then
    SIZE=$(du -sh "search.bleve-$i" | cut -f1)
    DOCS=$(cat "shard-$i.jsonl" | wc -l)
    printf "   shard-%d: %8s (%5d docs)\n" "$i" "$SIZE" "$DOCS"
  fi
done
echo ""

# Step 8: Restart services
echo "7️⃣  Restarting shard services..."
if docker compose -f docker-compose.yml ps shard-0 > /dev/null 2>&1; then
  echo "   Stopping shards..."
  docker compose -f docker-compose.yml stop shard-{0..7}
  
  echo "   Starting shards with new indexes..."
  docker compose -f docker-compose.yml up -d shard-{0..7}
  
  echo "✅ Shards restarted"
else
  echo "⏭️  Docker services not running (skipping restart)"
fi
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🎉 PHASE 6 DAY 1 COMPLETE!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Next steps:"
echo "  1. Test vector retrieval: ./test-vectors.sh"
echo "  2. Verify embeddings: curl 'localhost:8090/search?q=test'"
echo "  3. Ready for Day 2: True semantic search!"
echo ""