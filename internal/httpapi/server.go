// Package httpapi wires HTTP routes to the domain code.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/sale"
)

type Server struct {
	pool           *pgxpool.Pool
	sale           *sale.Service
	log            *slog.Logger
	requestTimeout time.Duration
}

func New(pool *pgxpool.Pool, svc *sale.Service, log *slog.Logger, requestTimeout time.Duration) *Server {
	return &Server{pool: pool, sale: svc, log: log, requestTimeout: requestTimeout}
}

// Handler returns the router. Go 1.22's ServeMux understands method and
// path-parameter patterns ("POST /drops/{id}/reservations"), so no router
// dependency is needed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /drops", s.createDrop)
	mux.HandleFunc("GET /drops/{id}", s.getDrop)
	mux.HandleFunc("POST /drops/{id}/reservations", s.reserve)
	return s.withTimeout(mux)
}

// withTimeout attaches a deadline to every request context. Pool acquisition
// and queries honour it, so a saturated pool fails fast instead of hanging.
func (s *Server) withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		s.log.Warn("healthz: database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /drops (admin; used by tests and the load generator).
func (s *Server) createDrop(w http.ResponseWriter, r *http.Request) {
	var in sale.CreateDropInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	d, err := s.sale.CreateDrop(r.Context(), in)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// GET /drops/{id}
func (s *Server) getDrop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st, err := s.sale.GetDrop(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type reserveRequest struct {
	Quantity int `json:"quantity"`
}

// POST /drops/{id}/reservations
//
// Idempotency-Key is required by the API contract now so clients send it from
// day one. Storing and replaying it lands in milestone 4.
func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	userID := r.Header.Get("X-User-Id")
	if userID == "" {
		writeError(w, http.StatusBadRequest, "missing_user", "X-User-Id header is required")
		return
	}
	if r.Header.Get("Idempotency-Key") == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	req := reserveRequest{Quantity: 1} // an empty body means quantity 1
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	res, err := s.sale.Reserve(r.Context(), id, userID, req.Quantity)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// writeDomainError maps service errors to HTTP. Anything that ran out of time
// (including waiting for a pool connection) is a 503 so clients know to back
// off and retry, never a hang and never a misleading 500.
func (s *Server) writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sale.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, sale.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, sale.ErrSoldOut):
		writeError(w, http.StatusConflict, "sold_out", err.Error())
	case errors.Is(err, sale.ErrPerUserLimit):
		writeError(w, http.StatusConflict, "per_user_limit", err.Error())
	case errors.Is(err, sale.ErrSaleNotStarted):
		writeError(w, http.StatusConflict, "sale_not_started", err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), pgconn.Timeout(err):
		writeError(w, http.StatusServiceUnavailable, "timeout", "server busy, retry with the same Idempotency-Key")
	default:
		s.log.Error("unhandled error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must be a positive integer")
		return 0, false
	}
	return id, true
}

// decodeJSON reads at most 64 KiB and rejects unknown fields, so typos like
// "quantitiy" fail loudly instead of silently defaulting.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
