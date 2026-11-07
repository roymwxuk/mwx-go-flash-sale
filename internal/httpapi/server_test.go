package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/httpapi"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/sale"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/testdb"
)

func newServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	strat, _ := sale.ParseStrategy("atomic")
	svc := sale.NewService(pool, strat, time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpapi.New(pool, svc, log, 2*time.Second).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// do sends a request and decodes the JSON response into a map.
func do(t *testing.T, method, url, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHealthz(t *testing.T) {
	pool := testdb.New(t, 2)
	srv := newServer(t, pool)

	if code, _ := do(t, "GET", srv.URL+"/healthz", "", nil); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// Once the DB is gone, healthz must report 503 rather than hang.
	pool.Close()
	if code, _ := do(t, "GET", srv.URL+"/healthz", "", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("status after pool close = %d, want 503", code)
	}
}

func TestReservationFlowOverHTTP(t *testing.T) {
	srv := newServer(t, testdb.New(t, 4))

	code, drop := do(t, "POST", srv.URL+"/drops", `{"name":"concert","total_stock":2}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("create drop: %d %v", code, drop)
	}
	dropURL := srv.URL + "/drops/" + jsonID(drop["id"])
	hdr := func(user string) map[string]string {
		return map[string]string{"X-User-Id": user, "Idempotency-Key": "k-" + user}
	}

	steps := []struct {
		name     string
		method   string
		url      string
		body     string
		headers  map[string]string
		wantCode int
		wantErr  string
	}{
		{"missing user", "POST", dropURL + "/reservations", "", map[string]string{"Idempotency-Key": "k"}, 400, "missing_user"},
		{"missing idempotency key", "POST", dropURL + "/reservations", "", map[string]string{"X-User-Id": "a"}, 400, "missing_idempotency_key"},
		{"unknown field", "POST", dropURL + "/reservations", `{"quantitiy":1}`, hdr("a"), 400, "invalid_json"},
		{"bad id", "POST", srv.URL + "/drops/abc/reservations", "", hdr("a"), 400, "invalid_id"},
		{"unknown drop", "POST", srv.URL + "/drops/999999/reservations", "", hdr("a"), 404, "not_found"},
		{"first hold (empty body = qty 1)", "POST", dropURL + "/reservations", "", hdr("a"), 201, ""},
		{"same user again", "POST", dropURL + "/reservations", `{"quantity":1}`, hdr("a"), 409, "per_user_limit"},
		{"second buyer takes last unit", "POST", dropURL + "/reservations", "", hdr("b"), 201, ""},
		// Sold-out wins over per-user limit: the lock-free pre-check runs first.
		{"sold out", "POST", dropURL + "/reservations", "", hdr("c"), 409, "sold_out"},
		{"invalid drop input", "POST", srv.URL + "/drops", `{"name":"x","total_stock":0}`, nil, 400, "invalid_input"},
	}
	for _, s := range steps {
		code, body := do(t, s.method, s.url, s.body, s.headers)
		if code != s.wantCode || (s.wantErr != "" && body["error"] != s.wantErr) {
			t.Errorf("%s: got %d %v, want %d %q", s.name, code, body, s.wantCode, s.wantErr)
		}
	}

	code, state := do(t, "GET", dropURL, "", nil)
	if code != 200 || state["remaining"] != float64(0) || state["held"] != float64(2) {
		t.Errorf("drop state: %d %v", code, state)
	}
}

// JSON numbers decode to float64 in a map[string]any.
func jsonID(v any) string {
	f, _ := v.(float64)
	return strconv.FormatInt(int64(f), 10)
}
