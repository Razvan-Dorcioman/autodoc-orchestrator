// Command server runs the test service.
package main

import (
	"net/http"
	"os"
	"time"

	"example.com/svc/internal/api"
)

func main() {
	srv := &http.Server{
		Addr:        ":" + envOr("PORT", "8080"),
		Handler:     api.NewRouter(),
		ReadTimeout: 10 * time.Second,
	}
	_ = os.Getenv("DEBUG")
	_ = srv.ListenAndServe()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
