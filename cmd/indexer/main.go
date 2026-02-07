package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Devanshusharma2005/distributed-search/internal/index"
)

func main() {
	var (
		jsonlPath = flag.String("input", "docs.jsonl", "path to JSONL docs")
		indexPath = flag.String("index", "search.bleve", "base index path")
		shardID   = flag.Int("shard-id", -1, "shard ID for this index (-1 = unsharded mode)")
		batchSize = flag.Int("batch-size", 1000, "batch size for indexing")
		maxDocs   = flag.Int("max-docs", 0, "max docs to index (0=all)")
	)
	flag.Parse()

	finalIndexPath := *indexPath
	if *shardID >= 0 {
		finalIndexPath = fmt.Sprintf("%s-%d", *indexPath, *shardID)
		log.Printf("🔀 Shard mode: indexing shard-%d → %s", *shardID, finalIndexPath)
	} else {
		log.Printf("📦 Unsharded mode → %s", finalIndexPath)
	}

	log.Printf("🚀 Starting indexer | input=%s index=%s batch=%d max=%d",
		*jsonlPath, finalIndexPath, *batchSize, *maxDocs)

	indexer, err := index.NewIndexer(finalIndexPath)
	if err != nil {
		log.Fatalf("init indexer: %v", err)
	}
	defer indexer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("🛑 Shutdown requested...")
		cancel()
	}()

	start := time.Now()
	if err := indexer.IndexJSONL(ctx, *jsonlPath, *batchSize, *maxDocs); err != nil {
		log.Fatalf("indexing failed: %v", err)
	}
	log.Printf("🎉 Indexer complete in %v!", time.Since(start))
}