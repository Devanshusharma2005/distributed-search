package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/Devanshusharma2005/distributed-search/internal/index"
	"github.com/blevesearch/bleve/v2"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// we'll need this for grafana later;

var (
	queryLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "search_query_latency_seconds",
		Help:    "Search query latency distribution",
		Buckets: prometheus.DefBuckets, // 0.005, 0.01, 0.025, 0.05, ... 10
	}, []string{"status"})

	queriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "search_queries_total",
		Help: "Total search queries processed",
	}, []string{"status"})
)

func init() {
	prometheus.MustRegister(queryLatency)
	prometheus.MustRegister(queriesTotal)
}


func main() {
	indexPath := flag.String("index", "search.bleve", "path to Bleve index")
	port := flag.String("port", "8080", "HTTP server port")
	flag.Parse()

	idx, err := index.NewIndexer(*indexPath)
	if err != nil {
		log.Fatalf("❌ load index: %v", err)
	}
	defer idx.Close()

	// Log index stats
	count, _ := idx.Index.DocCount()
	log.Printf("🚀 Searcher ready at :%s | %d docs indexed", *port, count)

	// Routes
	r := mux.NewRouter()
	r.HandleFunc("/search", searchHandler(idx.Index)).Methods("GET")
	r.Handle("/metrics", promhttp.Handler())
	r.HandleFunc("/health", healthHandler).Methods("GET")

	log.Fatal(http.ListenAndServe(":"+*port, r))
}


// searchHandler — the meat of Phase 1 Milestone 4.
// Takes a "q" query param, runs BM25 search, returns JSON with highlights.

func searchHandler(index bleve.Index) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Extract query
		q := r.URL.Query().Get("q")
		if q == "" {
			http.Error(w, `{"error": "missing 'q' parameter"}`, http.StatusBadRequest)
			queriesTotal.WithLabelValues("error").Inc()
			queryLatency.WithLabelValues("error").Observe(time.Since(start).Seconds())
			return
		}

		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 20
		}

		
		query := bleve.NewQueryStringQuery(q)
		searchReq := bleve.NewSearchRequest(query)
		searchReq.Size = limit
		searchReq.Fields = []string{"title", "body"} 
		searchReq.Highlight = bleve.NewHighlight()  

		
		res, err := index.Search(searchReq)
		if err != nil {
			http.Error(w, `{"error": "`+err.Error()+`"}`, http.StatusInternalServerError)
			queriesTotal.WithLabelValues("error").Inc()
			queryLatency.WithLabelValues("error").Observe(time.Since(start).Seconds())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)

		// Metrics
		queriesTotal.WithLabelValues("success").Inc()
		queryLatency.WithLabelValues("success").Observe(time.Since(start).Seconds())

		log.Printf("✅ '%s' → %d hits in %v", q, len(res.Hits), time.Since(start))
	}
}


// healthHandler (we gonna need this later)


func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}