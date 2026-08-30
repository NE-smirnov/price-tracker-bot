package bot

import (
	"sync"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// step is a node of the per-user conversation state machine.
type step string

const (
	stepIdle          step = "idle"
	stepAwaitURL      step = "await_url"
	stepAwaitTarget   step = "await_target"
	stepAwaitInterval step = "await_interval"
	stepAwaitCurrency step = "await_currency"
)

// draft accumulates the answers of the /add flow.
type draft struct {
	URL      string
	Target   *domain.Money
	Interval time.Duration
}

// session is the conversation state of a single Telegram user.
type session struct {
	Step      step
	Draft     draft
	UpdatedAt time.Time
}

// sessionStore keeps conversation state in memory with a TTL, so an abandoned
// /add flow does not trap the user forever. It is safe for concurrent use:
// Telegram updates for different chats are handled in parallel goroutines.
type sessionStore struct {
	mu  sync.Mutex
	m   map[int64]*session
	ttl time.Duration
	now func() time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &sessionStore{
		m:   make(map[int64]*session),
		ttl: ttl,
		now: time.Now,
	}
}

// get returns the live session for a user, resetting it when it has expired.
func (s *sessionStore) get(userID int64) session {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.m[userID]
	if !ok || s.now().Sub(cur.UpdatedAt) > s.ttl {
		delete(s.m, userID)
		return session{Step: stepIdle}
	}
	return *cur
}

// set stores the session for a user.
func (s *sessionStore) set(userID int64, sess session) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess.UpdatedAt = s.now()
	s.m[userID] = &sess
}

// reset drops any conversation state for a user.
func (s *sessionStore) reset(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, userID)
}

// gc removes expired sessions and returns how many were dropped.
func (s *sessionStore) gc() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	dropped := 0
	for id, sess := range s.m {
		if now.Sub(sess.UpdatedAt) > s.ttl {
			delete(s.m, id)
			dropped++
		}
	}
	return dropped
}
