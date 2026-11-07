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
	mux.HandleFunc("POST /reservations/{id}/confirm", s.confirm)
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
func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	userID, key, ok := callerHeaders(w, r)
	if !ok {
		return
	}
	req := reserveRequest{Quantity: 1} // an empty body means quantity 1
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	res, err := s.sale.Reserve(r.Context(), id, userID, req.Quantity, key)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeResult(w, res)
}

// POST /reservations/{id}/confirm
func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	userID, key, ok := callerHeaders(w, r)
	if !ok {
		return
	}
	res, err := s.sale.Confirm(r.Context(), id, userID, key)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeResult(w, res)
}

func callerHeaders(w http.ResponseWriter, r *http.Request) (userID, key string, ok bool) {
	userID = r.Header.Get("X-User-Id")
	key = r.Header.Get("Idempotency-Key")
	switch {
	case userID == "":
		writeError(w, http.StatusBadRequest, "missing_user", "X-User-Id header is required")
		return "", "", false
	case key == "":
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return "", "", false
	}
	return userID, key, true
}

// writeResult sends a stored-or-fresh response byte for byte. Replays are
// marked so clients and tests can tell them apart without parsing the body.
func writeResult(w http.ResponseWriter, res sale.Result) {
	w.Header().Set("Content-Type", "application/json")
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
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
	case errors.Is(err, sale.ErrIdempotencyMismatch):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", err.Error())
	case errors.Is(err, sale.ErrNotHeld):
		writeError(w, http.StatusConflict, "reservation_not_held", err.Error())
	case errors.Is(err, sale.ErrHoldExpired):
		writeError(w, http.StatusGone, "hold_expired", err.Error())
	case errors.Is(err, sale.ErrPaymentPending):
		writeError(w, http.StatusServiceUnavailable, "payment_pending", err.Error())
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
