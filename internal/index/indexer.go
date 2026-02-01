package index

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/Devanshusharma2005/distributed-search/internal/model"
)

type Indexer struct {
	Index bleve.Index
	path  string
}

func NewIndexer(indexPath string) (*Indexer, error) {
	// Bleve auto-handles your JSON struct fields PERFECTLY with defaults
	mapping := bleve.NewIndexMapping()

	index, err := bleve.Open(indexPath)
	if err != nil {
		index, err = bleve.New(indexPath, mapping)
		if err != nil {
			return nil, fmt.Errorf("create bleve index: %w", err)
		}
		log.Printf("📁 Created new index at %s", indexPath)
	} else {
		log.Printf("📂 Opened existing index at %s", indexPath)
	}

	return &Indexer{Index: index, path: indexPath}, nil
}

func (idx *Indexer) IndexJSONL(ctx context.Context, jsonlPath string, batchSize int, maxDocs int) error {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	batch := idx.Index.NewBatch()

	var indexed, skipped int
	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			log.Println("⚠️ Context cancelled, flushing remaining batch...")
			goto flush
		default:
		}

		var doc model.Doc
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("⚠️ decode error (skipping): %v", err)
			skipped++
			continue
		}

		if len(strings.TrimSpace(doc.Body)) == 0 {
			skipped++
			continue
		}

		// Bleve auto-indexes ALL JSON fields (title, body, id) - PERFECT!
		batch.Index(doc.ID, doc)
		indexed++

		if indexed%batchSize == 0 {
			if err := idx.Index.Batch(batch); err != nil {
				return fmt.Errorf("batch flush at %d: %w", indexed, err)
			}
			batch = idx.Index.NewBatch()
			log.Printf("📈 Indexed %d docs (%d skipped) | %.0f docs/sec",
				indexed, skipped, float64(indexed)/time.Since(start).Seconds())
		}

		if maxDocs > 0 && indexed >= maxDocs {
			log.Printf("🛑 Hit max-docs cap (%d)", maxDocs)
			break
		}
	}

flush:
	if batch.Size() > 0 {
		if err := idx.Index.Batch(batch); err != nil {
			return fmt.Errorf("final batch flush: %w", err)
		}
	}

	elapsed := time.Since(start)
	count, _ := idx.Index.DocCount()
	log.Printf("✅ Indexing complete! %d indexed, %d skipped in %v (%.0f docs/sec)",
		indexed, skipped, elapsed, float64(indexed)/elapsed.Seconds())
	log.Printf("📊 Index holds %d searchable documents", count)

	return nil
}

func (idx *Indexer) Close() error {
	return idx.Index.Close()
}