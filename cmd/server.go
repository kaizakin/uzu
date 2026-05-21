package main

import (
	"database/sql"
	"log"
	"net/http"
	"time"
)

type server struct {
	db     *sql.DB
	logger *log.Logger
}

func newServer(db *sql.DB, logger *log.Logger) *server {
	return &server{db: db, logger: logger}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /items", s.listItems)
	mux.HandleFunc("POST /items", s.createItem)
	mux.HandleFunc("GET /items/", s.getItem)
	mux.HandleFunc("PUT /items/", s.putItem)
	mux.HandleFunc("DELETE /items/", s.deleteItem)
	return requestLogger(s.logger, mux)
}

func requestLogger(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started).Round(time.Microsecond))
	})
}
