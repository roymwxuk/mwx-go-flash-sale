package sale_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/sale"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/testdb"
)

// --- unit tests (no database) ---

func TestCreateDropInputValidate(t *testing.T) {
	neg := int64(-1)
	cases := map[string]struct {
		in      sale.CreateDropInput
		wantErr bool
	}{
		"defaults applied":  {sale.CreateDropInput{Name: "x", TotalStock: 5}, false},
		"blank name":        {sale.CreateDropInput{Name: "  ", TotalStock: 5}, true},
		"zero stock":        {sale.CreateDropInput{Name: "x"}, true},
		"limit above stock": {sale.CreateDropInput{Name: "x", TotalStock: 2, PerUserLimit: 3}, true},
		"negative limit":    {sale.CreateDropInput{Name: "x", TotalStock: 2, PerUserLimit: -1}, true},
		"negative price":    {sale.CreateDropInput{Name: "x", TotalStock: 2, PriceMinor: &neg}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := tc.in
			err := in.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, sale.ErrInvalidInput) {
				t.Fatalf("want ErrInvalidInput, got %v", err)
			}
			if err == nil && (in.PerUserLimit != 1 || *in.PriceMinor != 1000) {
				t.Fatalf("defaults not applied: %+v", in)
			}
		})
	}
}

func TestParseStrategy(t *testing.T) {
	for _, name := range []string{"naive", "atomic"} {
		s, err := sale.ParseStrategy(name)
		if err != nil || s.Name() != name {
			t.Fatalf("ParseStrategy(%q) = %v, %v", name, s, err)
		}
	}
	if _, err := sale.ParseStrategy("magic"); err == nil {
		t.Fatal("unknown strategy should error")
	}
}

// --- sequential integration tests ---

func TestReserveSequential(t *testing.T) {
	for _, strategy := range sale.StrategyNames() {
		t.Run(strategy, func(t *testing.T) {
			ctx := context.Background()
			pool := testdb.New(t, 4)
			svc, dropID := newDrop(t, pool, strategy, 2)

			r, err := svc.Reserve(ctx, dropID, "a", 1)
			if err != nil || r.Status != "held" || r.Quantity != 1 {
				t.Fatalf("first hold: %+v, %v", r, err)
			}
			if !r.ExpiresAt.After(time.Now()) {
				t.Errorf("expires_at %v should be in the future", r.ExpiresAt)
			}
			if _, err := svc.Reserve(ctx, dropID, "a", 1); !errors.Is(err, sale.ErrPerUserLimit) {
				t.Errorf("second hold same user: want ErrPerUserLimit, got %v", err)
			}
			if _, err := svc.Reserve(ctx, dropID, "b", 2); !errors.Is(err, sale.ErrPerUserLimit) {
				t.Errorf("quantity above limit: want ErrPerUserLimit, got %v", err)
			}
			if _, err := svc.Reserve(ctx, dropID, "b", 1); err != nil {
				t.Errorf("last unit: %v", err)
			}
			if _, err := svc.Reserve(ctx, dropID, "c", 1); !errors.Is(err, sale.ErrSoldOut) {
				t.Errorf("after sell-out: want ErrSoldOut, got %v", err)
			}
			if _, err := svc.Reserve(ctx, 999999, "a", 1); !errors.Is(err, sale.ErrNotFound) {
				t.Errorf("unknown drop: want ErrNotFound, got %v", err)
			}

			st, err := svc.GetDrop(ctx, dropID)
			if err != nil || st.Remaining != 0 || st.Held != 2 {
				t.Errorf("state = %+v, %v; want remaining 0, held 2", st, err)
			}
			if rep := check(t, pool, dropID); !rep.OK() {
				t.Errorf("violations: %+v", rep.Violations)
			}
		})
	}
}

func TestReserveBeforeSaleStarts(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 4)
	strat, _ := sale.ParseStrategy("atomic")
	svc := sale.NewService(pool, strat, time.Minute)

	later := time.Now().Add(time.Hour)
	d, err := svc.CreateDrop(ctx, sale.CreateDropInput{Name: "later", TotalStock: 1, SaleStartsAt: &later})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve(ctx, d.ID, "a", 1); !errors.Is(err, sale.ErrSaleNotStarted) {
		t.Fatalf("want ErrSaleNotStarted, got %v", err)
	}
}
