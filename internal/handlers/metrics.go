package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gablue316/ref-ledger-monitor/internal/model"
)

// MetricsReader allows handler tests without connecting to MongoDB.
type MetricsReader interface {
	LastMinute(context.Context, time.Time) (model.LastMinuteResponse, error)
}

func LastMinute(reader MetricsReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
			return
		}

		// Keep below the page's 8-second fetch timeout and the server write timeout.
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		data, err := reader.LastMinute(ctx, time.Now())
		if err != nil {
			log.Printf("Read monitor metrics: %v", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			if r.Method != http.MethodHead {
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "Unable to read metrics; check the monitor server log",
				})
			}
			return
		}

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := json.NewEncoder(w).Encode(data); err != nil {
			log.Print("Unable to encode monitor metrics response")
		}
	}
}
