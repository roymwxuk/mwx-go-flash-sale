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
	svc := sale.NewService(pool, strat, time.Minute, nil)
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
	hdr := func(user, key string) map[string]string {
		return map[string]string{"X-User-Id": user, "Idempotency-Key": key}
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
		{"unknown field", "POST", dropURL + "/reservations", `{"quantitiy":1}`, hdr("a", "a1"), 400, "invalid_json"},
		{"bad id", "POST", srv.URL + "/drops/abc/reservations", "", hdr("a", "a1"), 400, "invalid_id"},
		{"unknown drop", "POST", srv.URL + "/drops/999999/reservations", "", hdr("a", "a1"), 404, "not_found"},
		{"key too long", "POST", dropURL + "/reservations", "", hdr("a", strings.Repeat("k", 256)), 400, "invalid_input"},
		{"first hold (empty body = qty 1)", "POST", dropURL + "/reservations", "", hdr("a", "a1"), 201, ""},
		// Same key, same meaning ({"quantity":1} == empty body): replayed 201.
		{"retry with same key", "POST", dropURL + "/reservations", `{"quantity":1}`, hdr("a", "a1"), 201, ""},
		{"same key, different quantity", "POST", dropURL + "/reservations", `{"quantity":2}`, hdr("a", "a1"), 422, "idempotency_key_reused"},
		{"same user, new key", "POST", dropURL + "/reservations", "", hdr("a", "a2"), 409, "per_user_limit"},
		{"second buyer takes last unit", "POST", dropURL + "/reservations", "", hdr("b", "b1"), 201, ""},
		// Sold-out wins over per-user limit: the lock-free pre-check runs first.
		{"sold out", "POST", dropURL + "/reservations", "", hdr("c", "c1"), 409, "sold_out"},
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

func TestConfirmOverHTTP(t *testing.T) {
	srv := newServer(t, testdb.New(t, 4))
	_, drop := do(t, "POST", srv.URL+"/drops", `{"name":"goods","total_stock":5}`, nil)
	h := func(user, key string) map[string]string {
		return map[string]string{"X-User-Id": user, "Idempotency-Key": key}
	}
	_, hold := do(t, "POST", srv.URL+"/drops/"+jsonID(drop["id"])+"/reservations", "", h("a", "r1"))
	confirmURL := srv.URL + "/reservations/" + jsonID(hold["id"]) + "/confirm"

	if code, body := do(t, "POST", confirmURL, "", h("b", "c1")); code != 404 {
		t.Errorf("other user's reservation: %d %v, want 404", code, body)
	}
	if code, body := do(t, "POST", confirmURL, "", h("a", "r1")); code != 422 {
		t.Errorf("reserve key reused for confirm: %d %v, want 422", code, body)
	}

	code, first, hdr1 := doH(t, "POST", confirmURL, h("a", "c1"))
	if code != 200 || !strings.Contains(first, `"status":"confirmed"`) || hdr1.Get("Idempotent-Replayed") != "" {
		t.Fatalf("confirm: %d %s", code, first)
	}
	code, again, hdr2 := doH(t, "POST", confirmURL, h("a", "c1"))
	if code != 200 || again != first || hdr2.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %q vs %q, replayed=%q", code, again, first, hdr2.Get("Idempotent-Replayed"))
	}
	// A *different* key for an already confirmed reservation is not an
	// error either: confirm is idempotent by state, not only by key.
	if code, body := do(t, "POST", confirmURL, "", h("a", "c2")); code != 200 || body["status"] != "confirmed" {
		t.Errorf("confirm with a new key: %d %v", code, body)
	}
}

// doH returns the raw body and headers, for byte-level replay checks.
func doH(t *testing.T, method, url string, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// JSON numbers decode to float64 in a map[string]any.
func jsonID(v any) string {
	f, _ := v.(float64)
	return strconv.FormatInt(int64(f), 10)
}
