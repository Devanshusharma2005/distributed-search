package search

import (
	"github.com/blevesearch/bleve/v2/search"
)

func FormatHit(hit *search.DocumentMatch) map[string]interface{} {
	result := map[string]interface{}{
		"id":        hit.ID,
		"score":     hit.Score,
		"title":     hit.Fields["title"],
		"fragments": map[string][]string{},
	}

	for field, frags := range hit.Fragments {
		result["fragments"].(map[string][]string)[field] = frags
	}

	return result
}
