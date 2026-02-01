package model

import "time"

type Doc struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	Tokens  []string  `json:"tokens,omitempty"`
	Indexed time.Time `json:"indexed_at"`
}