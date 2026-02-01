package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Devanshusharma2005/distributed-search/internal/model"
)


type page struct {
	Title    string   `xml:"title"`
	Revision revision `xml:"revision"`
}

type revision struct {
	Text string `xml:"text"`
}


const (
	defaultWorkers = 8
	defaultMaxDocs = 50000 
	batchSize      = 1000  
	minBodyLen     = 50    
	maxBodyLen     = 2000  
)


func cleanWikiText(raw string) string {
	r := strings.NewReplacer(
		"'''", "",
		"''", "",
		"[[", "",
		"]]", "",
		"{{", "",
		"}}", "",
		"[", "",
		"]", "",
		"\n", " ",
		"\t", " ",
	)
	text := r.Replace(raw)


	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}

	if len(text) > maxBodyLen {
		text = text[:maxBodyLen]
	}
	return strings.TrimSpace(text)
}


func worker(ctx context.Context, id int, in <-chan page, out chan<- model.Doc, counter *atomic.Int64) {
    log.Printf("[Worker %d] Starting...", id)
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-in:
			if !ok {
				return 
			}

			body := cleanWikiText(raw.Revision.Text)
			if len(body) < minBodyLen {
				continue 
			}

			seq := counter.Add(1) 

			out <- model.Doc{
				ID:      fmt.Sprintf("wiki_%d", seq),
				Title:   raw.Title,
				Body:    body,
				Indexed: time.Now(),
			}
		}
	}
}



func main() {
	inputFile := flag.String("input", "data.xml", "path to Wikipedia XML dump")
	outputFile := flag.String("output", "docs.jsonl", "path to output JSONL file")
	maxDocs := flag.Int("max-docs", defaultMaxDocs, "max docs to index (0 = no limit)")
	numWorkers := flag.Int("workers", defaultWorkers, "number of parser worker goroutines")
	flag.Parse()

	log.Printf("🚀 Starting ingester | input=%s output=%s max-docs=%d workers=%d",
		*inputFile, *outputFile, *maxDocs, *numWorkers)

	start := time.Now()

	f, err := os.Open(*inputFile)
	if err != nil {
		log.Fatalf("open input: %v", err)
	}
	defer f.Close()

	out, err := os.Create(*outputFile)
	if err != nil {
		log.Fatalf("create output: %v", err)
	}
	defer out.Close()

	pageCh := make(chan page, 512)
	docCh := make(chan model.Doc, 512)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var counter atomic.Int64 

	var wg sync.WaitGroup
	for i := 0; i < *numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			worker(ctx, id, pageCh, docCh, &counter)
		}(i)
	}

	go func() {
		wg.Wait()
		close(docCh)
	}()

	
	go func() {
		defer close(pageCh) 

		decoder := xml.NewDecoder(f)
		var pagesRead int

		for {
			tok, err := decoder.Token()
			if err != nil {
				break 
			}

			se, ok := tok.(xml.StartElement)
			if !ok || se.Name.Local != "page" {
				continue
			}

			var p page
			if err := decoder.DecodeElement(&p, &se); err != nil {
				log.Printf("⚠️  decode error (skipping): %v", err)
				continue
			}

			pagesRead++
			if pagesRead%10000 == 0 {
				log.Printf("📄 Read %d pages from XML...", pagesRead)
			}

			if *maxDocs > 0 && pagesRead >= *maxDocs {
				log.Printf("📄 Hit max-docs cap (%d), stopping reader.", *maxDocs)
				break
			}

			pageCh <- p 
		}

		log.Printf("📄 Reader finished. Total pages pulled from XML: %d", pagesRead)
	}()

	enc := json.NewEncoder(out)
	batch := make([]model.Doc, 0, batchSize)
	var totalWritten int

	for doc := range docCh {
		batch = append(batch, doc)

		if len(batch) >= batchSize {
			for _, d := range batch {
				if err := enc.Encode(d); err != nil {
					log.Printf("⚠️  encode error: %v", err)
				}
			}
			totalWritten += len(batch)
			batch = batch[:0] 

			if totalWritten%5000 == 0 {
				log.Printf("✍️  Wrote %d docs to %s...", totalWritten, *outputFile)
			}
		}
	}

	for _, d := range batch {
		if err := enc.Encode(d); err != nil {
			log.Printf("⚠️  encode error: %v", err)
		}
	}
	totalWritten += len(batch)

	elapsed := time.Since(start)
	log.Printf("✅ Done! %d docs indexed in %v (%.0f docs/sec) → %s",
		totalWritten, elapsed, float64(totalWritten)/elapsed.Seconds(), *outputFile)
}