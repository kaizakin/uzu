package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

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

	app := newServer(db, logger)

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           app.routes(),
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
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
