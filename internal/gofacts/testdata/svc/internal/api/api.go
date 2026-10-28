// Package api is the HTTP layer of the test service.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

const (
	defaultLimit = 5
	maxLimit     = 20
)

// Color is an allowed filter value.
type Color string

const (
	Red  Color = "red"
	Blue Color = "blue"
)

var weights = map[Color]int{Red: 1, Blue: 2}

// Item is one entry.
type Item struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"` // empty when unknown
}

type ItemsResponse struct {
	Items []Item `json:"items"`
}

type ErrorResponse struct {
	Code string `json:"code"`
}

func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /items/{id}", item)
	mux.Handle("POST /files/{path...}", http.NotFoundHandler())
	mux.HandleFunc("/orders", orders)
	mux.HandleFunc("/notes", notes)
	return recoverer(mux)
}

func index(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := defaultLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n > maxLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be at most %d", maxLimit))
			return
		}
		limit = n
	}
	_ = limit
	writeJSON(w, http.StatusOK, ItemsResponse{})
}

func item(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must be an integer")
		return
	}
	if r.URL.Query().Has("color") && weights[Color(r.URL.Query().Get("color"))] == 0 {
		writeError(w, http.StatusBadRequest, "invalid_color", "unknown color")
		return
	}
	if id > 100 {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}
	writeJSON(w, http.StatusOK, Item{ID: id})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Code: code})
}
