package services

import (
	"errors"
	"sync"
)

var (
	ErrImportUserBusy = errors.New("too many concurrent imports for this user")
	ErrImportBusy     = errors.New("remote import capacity exhausted")
)

const (
	importMaxPerUser  = 2
	importMaxInFlight = 8
	importMinBudget   = int64(256 << 20)
	importReserveStep = int64(1 << 20)
)

// importLimiter bounds remote downloads independently of the image-processing
// slot: in-flight permits per user ID and globally, plus a spool byte budget
// that is reserved as bytes arrive rather than up front.
type importLimiter struct {
	mu       sync.Mutex
	users    map[int]*importUsage
	inFlight int
	reserved int64
}

type importUsage struct {
	inFlight int
	reserved int64
}

type ImportTicket struct {
	limiter     *importLimiter
	userID      int
	maxFileSize int64
	reserved    int64
	once        sync.Once
}

func newImportLimiter() *importLimiter {
	return &importLimiter{users: map[int]*importUsage{}}
}

// Begin fails instead of waiting. Permits are keyed by user ID so that minting
// more tokens does not multiply them.
func (l *importLimiter) Begin(userID int, maxFileSize int64) (*ImportTicket, error) {
	ticket := &ImportTicket{limiter: l, userID: userID, maxFileSize: maxFileSize}
	l.mu.Lock()
	defer l.mu.Unlock()
	usage := l.users[userID]
	if usage != nil && usage.inFlight >= importMaxPerUser {
		return nil, ErrImportUserBusy
	}
	if l.inFlight >= importMaxInFlight {
		return nil, ErrImportBusy
	}
	if usage == nil {
		usage = &importUsage{}
		l.users[userID] = usage
	}
	usage.inFlight++
	l.inFlight++
	return ticket, nil
}

// Grow ensures at least need bytes are reserved, in steps of at most 1 MiB, so
// a slow sender reserves little more than it has delivered. Requires
// need ≤ maxFileSize+1.
func (t *ImportTicket) Grow(need int64) error {
	// Only the goroutine that owns the ticket changes t.reserved, so it can be read unlocked.
	if t.reserved >= need {
		return nil
	}
	budget, userCap := t.budget(), t.userCap()
	l := t.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	usage := l.users[t.userID]
	for t.reserved < need {
		step := min(importReserveStep, t.maxFileSize+1-t.reserved)
		if step <= 0 || l.reserved+step > budget || usage.reserved+step > userCap {
			return ErrImportBusy
		}
		l.reserved += step
		usage.reserved += step
		t.reserved += step
	}
	return nil
}

func (t *ImportTicket) budget() int64 {
	return max(importMinBudget, 2*t.maxFileSize)
}

// userCap ≤ budget/2 because budget ≥ 2×maxFileSize, so one user never holds the whole budget.
func (t *ImportTicket) userCap() int64 {
	return max(t.maxFileSize, t.budget()/4)
}

func (t *ImportTicket) End() {
	t.once.Do(func() {
		l := t.limiter
		l.mu.Lock()
		defer l.mu.Unlock()
		usage := l.users[t.userID]
		l.reserved -= t.reserved
		usage.reserved -= t.reserved
		t.reserved = 0
		l.inFlight--
		usage.inFlight--
		if usage.inFlight == 0 {
			delete(l.users, t.userID)
		}
	})
}
