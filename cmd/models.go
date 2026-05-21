package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type item struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type itemInput struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type apiError struct {
	Error string `json:"error"`
}

type itemScanner interface {
	Scan(dest ...any) error
}

func scanItem(s itemScanner) (item, error) {
	var it item
	var createdAt, updatedAt string
	if err := s.Scan(&it.Key, &it.Value, &createdAt, &updatedAt); err != nil {
		return item{}, err
	}

	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return item{}, fmt.Errorf("parse created_at: %w", err)
	}
	updated, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return item{}, fmt.Errorf("parse updated_at: %w", err)
	}
	it.CreatedAt = created
	it.UpdatedAt = updated
	return it, nil
}

func itemKeyFromPath(path string) (string, bool) {
	key := strings.TrimSpace(strings.TrimPrefix(path, "/items/"))
	return key, key != "" && !strings.Contains(key, "/")
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload != nil {
		_ = json.NewEncoder(w).Encode(payload)
	}
}
