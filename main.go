package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
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

type server struct {
	db     *sql.DB
	logger *log.Logger
}

func main() {
	listen := flag.String("listen", envOrDefault("LISTEN_ADDR", ":8080"), "HTTP listen address")
	dbPath := flag.String("db", envOrDefault("DB_PATH", "/data/app.db"), "SQLite database path")
	flag.Parse()

	if port := os.Getenv("PORT"); port != "" && *listen == ":8080" {
		*listen = ":" + port
	}

	logger := log.New(os.Stdout, "sqlite-api ", log.Ldate|log.Ltime|log.Lmicroseconds|log.LUTC)

	db, err := openDatabase(*dbPath)
	if err != nil {
		logger.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := migrate(db); err != nil {
		logger.Fatalf("migrate database: %v", err)
	}

	app := &server{db: db, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.healthz)
	mux.HandleFunc("GET /items", app.listItems)
	mux.HandleFunc("POST /items", app.createItem)
	mux.HandleFunc("GET /items/", app.getItem)
	mux.HandleFunc("PUT /items/", app.putItem)
	mux.HandleFunc("DELETE /items/", app.deleteItem)

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           requestLogger(logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Printf("listening on %s, sqlite=%s", *listen, *dbPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Println("shutdown requested")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Fatalf("shutdown: %v", err)
	}
	logger.Println("shutdown complete")
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func openDatabase(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS items (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
`)
	return err
}

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
    value = excluded.value,
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

type itemScanner interface {
	Scan(dest ...any) error
}

func scanItem(scanner itemScanner) (item, error) {
	var it item
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(&it.Key, &it.Value, &createdAt, &updatedAt); err != nil {
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
	key := strings.TrimPrefix(path, "/items/")
	key = strings.TrimSpace(key)
	return key, key != "" && !strings.Contains(key, "/")
}

func decodeJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
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

func requestLogger(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started).Round(time.Microsecond))
	})
}
