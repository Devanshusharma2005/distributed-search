package search

import (
	"github.com/blevesearch/bleve/v2"
)

// FormatHit takes a Bleve search hit and formats it into a clean map
// structure ready for JSON serialization. Extracts score, fields, and
// highlighted fragments.
func FormatHit(hit *bleve.SearchHit, doc interface{}) map[string]interface{} {
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