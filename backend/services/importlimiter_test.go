package services

import (
	"errors"
	"testing"
)

const mib = int64(1 << 20)

func TestImportLimiterPerUserPermits(t *testing.T) {
	limiter := newImportLimiter()
	for range 2 {
		if _, err := limiter.Begin(1, 10*mib); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := limiter.Begin(1, 10*mib); !errors.Is(err, ErrImportUserBusy) {
		t.Fatalf("third import for one user: error = %v, want ErrImportUserBusy", err)
	}
	if _, err := limiter.Begin(2, 10*mib); err != nil {
		t.Fatalf("another user: %v", err)
	}
}

func TestImportLimiterGlobalPermits(t *testing.T) {
	limiter := newImportLimiter()
	for user := 1; user <= 8; user++ {
		if _, err := limiter.Begin(user, 10*mib); err != nil {
			t.Fatal(user, err)
		}
	}
	if _, err := limiter.Begin(9, 10*mib); !errors.Is(err, ErrImportBusy) {
		t.Fatalf("ninth user: error = %v, want ErrImportBusy", err)
	}
}

func TestImportLimiterGrowIncremental(t *testing.T) {
	limiter := newImportLimiter()
	ticket, err := limiter.Begin(1, 10*mib)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ need, reserved int64 }{
		{1, mib},
		{mib, mib},
		{mib + 1, 2 * mib},
		{10*mib + 1, 10*mib + 1},
	} {
		if err := ticket.Grow(step.need); err != nil {
			t.Fatal(err)
		}
		if ticket.reserved != step.reserved || limiter.reserved != step.reserved {
			t.Fatalf("Grow(%d): ticket=%d global=%d, want %d", step.need, ticket.reserved, limiter.reserved, step.reserved)
		}
	}
}

func TestImportLimiterPerUserByteCap(t *testing.T) {
	limiter := newImportLimiter()
	const maxFileSize = 1 << 30
	// B = 2 GiB, U = max(1 GiB, B/4) = 1 GiB.
	first, err := limiter.Begin(1, maxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Grow(maxFileSize); err != nil {
		t.Fatal(err)
	}
	second, err := limiter.Begin(1, maxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Grow(1); !errors.Is(err, ErrImportBusy) {
		t.Fatalf("user over byte cap: error = %v, want ErrImportBusy", err)
	}
	other, err := limiter.Begin(2, maxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Grow(maxFileSize); err != nil {
		t.Fatalf("other user: %v", err)
	}
}

func TestImportLimiterGlobalByteBudget(t *testing.T) {
	limiter := newImportLimiter()
	// B = 256 MiB, U = 64 MiB: four users fill the budget, the fifth cannot grow.
	for user := 1; user <= 4; user++ {
		ticket, err := limiter.Begin(user, 64*mib)
		if err != nil {
			t.Fatal(err)
		}
		if err := ticket.Grow(64 * mib); err != nil {
			t.Fatal(user, err)
		}
	}
	ticket, err := limiter.Begin(5, 64*mib)
	if err != nil {
		t.Fatal(err)
	}
	if err := ticket.Grow(1); !errors.Is(err, ErrImportBusy) {
		t.Fatalf("over global budget: error = %v, want ErrImportBusy", err)
	}
}

func TestImportLimiterEndIdempotent(t *testing.T) {
	limiter := newImportLimiter()
	ticket, err := limiter.Begin(1, 10*mib)
	if err != nil {
		t.Fatal(err)
	}
	if err := ticket.Grow(3 * mib); err != nil {
		t.Fatal(err)
	}
	ticket.End()
	ticket.End()
	if limiter.inFlight != 0 || limiter.reserved != 0 || len(limiter.users) != 0 {
		t.Fatalf("after End: inFlight=%d reserved=%d users=%d", limiter.inFlight, limiter.reserved, len(limiter.users))
	}
}
