package bot

import (
	"sync"
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	t.Parallel()

	s := newSessionStore(time.Minute)

	if got := s.get(1).Step; got != stepIdle {
		t.Fatalf("fresh session step = %q, want idle", got)
	}

	s.set(1, session{Step: stepAwaitTarget, Draft: draft{URL: "https://example.com/p/1"}})
	sess := s.get(1)
	if sess.Step != stepAwaitTarget || sess.Draft.URL != "https://example.com/p/1" {
		t.Fatalf("session not stored: %+v", sess)
	}

	s.reset(1)
	if got := s.get(1).Step; got != stepIdle {
		t.Fatalf("step after reset = %q, want idle", got)
	}
}

func TestSessionExpires(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := newSessionStore(10 * time.Minute)
	s.now = func() time.Time { return now }

	s.set(7, session{Step: stepAwaitURL})
	if got := s.get(7).Step; got != stepAwaitURL {
		t.Fatalf("step = %q, want await_url", got)
	}

	// A user who walked away mid-dialog must not stay stuck in it.
	now = now.Add(11 * time.Minute)
	if got := s.get(7).Step; got != stepIdle {
		t.Fatalf("expired session step = %q, want idle", got)
	}
}

func TestSessionGC(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := newSessionStore(5 * time.Minute)
	s.now = func() time.Time { return now }

	s.set(1, session{Step: stepAwaitURL})
	s.set(2, session{Step: stepAwaitURL})
	now = now.Add(6 * time.Minute)
	s.set(3, session{Step: stepAwaitURL}) // fresh

	if dropped := s.gc(); dropped != 2 {
		t.Fatalf("gc dropped %d sessions, want 2", dropped)
	}
	if got := s.get(3).Step; got != stepAwaitURL {
		t.Fatalf("gc removed a live session: step = %q", got)
	}
}

func TestSessionStoreConcurrentAccess(t *testing.T) {
	t.Parallel()

	s := newSessionStore(time.Minute)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := int64(i % 4)
			s.set(id, session{Step: stepAwaitURL})
			_ = s.get(id)
			if i%3 == 0 {
				s.reset(id)
			}
			s.gc()
		}(i)
	}
	wg.Wait()
}
