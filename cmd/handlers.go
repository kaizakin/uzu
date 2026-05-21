package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) listItems(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT key, value, created_at, updated_at FROM items ORDER BY key`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	defer rows.Close()

	items := make([]item, 0)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
			return
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *server) createItem(w http.ResponseWriter, r *http.Request) {
	var input itemInput
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	input.Key = strings.TrimSpace(input.Key)
	if input.Key == "" {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "key is required"})
		return
	}

	it, err := s.upsert(r.Context(), input.Key, input.Value)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, it)
}

func (s *server) getItem(w http.ResponseWriter, r *http.Request) {
	key, ok := itemKeyFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "item key is required"})
		return
	}

	it, err := s.find(r.Context(), key)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, apiError{Error: "item not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) putItem(w http.ResponseWriter, r *http.Request) {
	key, ok := itemKeyFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "item key is required"})
		return
	}

	var input itemInput
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}

	it, err := s.upsert(r.Context(), key, input.Value)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) deleteItem(w http.ResponseWriter, r *http.Request) {
	key, ok := itemKeyFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "item key is required"})
		return
	}

	result, err := s.db.ExecContext(r.Context(), `DELETE FROM items WHERE key = ?`, key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	if deleted == 0 {
		writeJSON(w, http.StatusNotFound, apiError{Error: "item not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) upsert(ctx context.Context, key, value string) (item, error) {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO items (key, value)
VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET
    value      = excluded.value,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
`, key, value)
	if err != nil {
		return item{}, err
	}
	return s.find(ctx, key)
}

func (s *server) find(ctx context.Context, key string) (item, error) {
	row := s.db.QueryRowContext(ctx, `SELECT key, value, created_at, updated_at FROM items WHERE key = ?`, key)
	return scanItem(row)
}
