package index

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/Devanshusharma2005/distributed-search/internal/embed"
	"github.com/Devanshusharma2005/distributed-search/internal/model"
)

type Indexer struct {
	Index       bleve.Index
	path        string
	embedClient *embed.OllamaClient 
}

func NewIndexer(indexPath string) (*Indexer, error) {
	return NewIndexerWithVectors(indexPath, nil)
}

func NewIndexerWithVectors(indexPath string, embedClient *embed.OllamaClient) (*Indexer, error) {
	index, err := bleve.Open(indexPath)
	if err == nil {
		log.Printf("Opened existing index at %s", indexPath)
		return &Indexer{Index: index, path: indexPath, embedClient: embedClient}, nil
	}

	mapping := bleve.NewIndexMapping()
	
	if embedClient != nil {
		docMapping := bleve.NewDocumentMapping()
		
		textFieldMapping := bleve.NewTextFieldMapping()
		docMapping.AddFieldMappingsAt("id", textFieldMapping)
		docMapping.AddFieldMappingsAt("title", textFieldMapping)
		docMapping.AddFieldMappingsAt("body", textFieldMapping)
		
		vectorFieldMapping := bleve.NewNumericFieldMapping()
		vectorFieldMapping.Store = true
		vectorFieldMapping.Index = false
		docMapping.AddFieldMappingsAt("title_vector", vectorFieldMapping)
		
		mapping.AddDocumentMapping("_default", docMapping)
		log.Printf("Creating index WITH vector field support")
	}

	index, err = bleve.New(indexPath, mapping)
	if err != nil {
		return nil, fmt.Errorf("create bleve index: %w", err)
	}

	log.Printf("Created new index at %s", indexPath)
	return &Indexer{Index: index, path: indexPath, embedClient: embedClient}, nil
}


func (idx *Indexer) IndexJSONL(ctx context.Context, jsonlPath string, batchSize int, maxDocs int) error {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	batch := idx.Index.NewBatch()

	var indexed, skipped, embedsSuccess, embedsFailed int
	start := time.Now()
	lastLog := time.Now()

	for {
		select {
		case <-ctx.Done():
			log.Println("Context cancelled, flushing remaining batch...")
			goto flush
		default:
		}

		var doc model.Doc
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("skip bad JSON line: %v", err)
			skipped++
			continue
		}

		if idx.embedClient != nil {
			embeddingText := doc.Title
			if embeddingText == "" {
				embeddingText = truncate(doc.Body, 100)
			}

			embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			embedding, err := idx.embedClient.GetEmbedding(embedCtx, embeddingText)
			cancel()

			if err != nil {
				embedsFailed++
				if embedsFailed <= 3 {
					log.Printf("Embedding failed for doc %s: %v", doc.ID, err)
				}
			} else {
				doc.TitleVector = embedding
				embedsSuccess++
			}
		}

		if err := batch.Index(doc.ID, doc); err != nil {
			log.Printf("batch.Index error: %v", err)
			continue
		}
		indexed++

		if batch.Size() >= batchSize {
			if err := idx.Index.Batch(batch); err != nil {
				return fmt.Errorf("commit batch: %w", err)
			}
			batch = idx.Index.NewBatch()
			
			// Progress logging
			if time.Since(lastLog) > 2*time.Second {
				elapsed := time.Since(start)
				rate := float64(indexed) / elapsed.Seconds()
				if idx.embedClient != nil {
					log.Printf("Progress: %d docs indexed (%.0f/sec, %d embeds OK, %d failed)",
						indexed, rate, embedsSuccess, embedsFailed)
				} else {
					log.Printf("Progress: %d docs indexed (%.0f/sec)",
						indexed, rate)
				}
				lastLog = time.Now()
			}
		}

		if maxDocs > 0 && indexed >= maxDocs {
			log.Printf("Reached max-docs limit (%d)", maxDocs)
			break
		}
	}

flush:
	if batch.Size() > 0 {
		if err := idx.Index.Batch(batch); err != nil {
			return fmt.Errorf("final batch: %w", err)
		}
	}

	elapsed := time.Since(start)
	docsPerSec := float64(indexed) / elapsed.Seconds()

	log.Printf("Indexing complete:")
	log.Printf("%d docs indexed, %d skipped", indexed, skipped)
	if idx.embedClient != nil {
		log.Printf(" %d embeddings generated, %d failed", embedsSuccess, embedsFailed)
	}
	log.Printf("%v elapsed (%.0f docs/sec)", elapsed, docsPerSec)

	return nil
}

func (idx *Indexer) Close() error {
	return idx.Index.Close()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}