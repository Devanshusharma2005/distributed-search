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

	"github.com/Devanshusharma2005/distributed-search/internal/embed"
	"github.com/Devanshusharma2005/distributed-search/internal/index"
)

func main() {
	var (
		jsonlPath   = flag.String("input", "docs.jsonl", "path to JSONL docs")
		indexPath   = flag.String("index", "search.bleve", "base index path")
		shardID     = flag.Int("shard-id", -1, "shard ID for this index (-1 = unsharded mode)")
		batchSize   = flag.Int("batch-size", 1000, "batch size for indexing")
		maxDocs     = flag.Int("max-docs", 0, "max docs to index (0=all)")
		ollamaURL   = flag.String("ollama", "http://localhost:11434", "Ollama API URL for embeddings")
		skipVectors = flag.Bool("skip-vectors", false, "skip embedding generation (keyword-only)")
	)
	flag.Parse()

	// Build final index path
	finalIndexPath := *indexPath
	if *shardID >= 0 {
		finalIndexPath = fmt.Sprintf("%s-%d", *indexPath, *shardID)
		log.Printf("🔀 Shard mode: indexing shard-%d → %s", *shardID, finalIndexPath)
	} else {
		log.Printf("📦 Unsharded mode → %s", finalIndexPath)
	}

	log.Printf("🚀 Starting indexer | input=%s index=%s batch=%d max=%d vectors=%v",
		*jsonlPath, finalIndexPath, *batchSize, *maxDocs, !*skipVectors)

	// Phase 6: Initialize embedding client (unless skipped)
	var embedClient *embed.OllamaClient
	if !*skipVectors {
		embedClient = embed.NewOllamaClient(*ollamaURL, "all-minilm")
		log.Printf("🤖 Embedding client initialized (url=%s, model=all-minilm)", *ollamaURL)

		// Test Ollama connectivity
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		testVec, err := embedClient.GetEmbedding(ctx, "test")
		cancel()

		if err != nil {
			log.Printf("⚠️  Ollama not reachable: %v (falling back to keyword-only)", err)
			embedClient = nil
		} else {
			log.Printf("✅ Ollama connected (test embedding: %d dims)", len(testVec))
		}
	} else {
		log.Printf("⏭️  Vector generation SKIPPED (keyword-only mode)")
	}

	// Create indexer (with or without vector support)
	var indexer *index.Indexer
	var err error
	
	if embedClient != nil {
		indexer, err = index.NewIndexerWithVectors(finalIndexPath, embedClient)
	} else {
		indexer, err = index.NewIndexer(finalIndexPath)
	}
	
	if err != nil {
		log.Fatalf("❌ Init indexer: %v", err)
	}
	defer indexer.Close()

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("🛑 Shutdown requested...")
		cancel()
	}()

	// Index documents
	start := time.Now()
	if err := indexer.IndexJSONL(ctx, *jsonlPath, *batchSize, *maxDocs); err != nil {
		log.Fatalf("❌ Indexing failed: %v", err)
	}
	
	log.Printf("🎉 Indexer complete in %v!", time.Since(start))
}