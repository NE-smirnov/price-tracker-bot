package notify

import "sync"

// attemptCounter counts delivery attempts per alert.
//
// It is in-process rather than in Redis: the count only has to stop a failing
// alert from circulating forever, and a restart re-arming the counter is
// harmless. Keeping it out of Redis also means a retry costs no round trip.
type attemptCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func newAttemptCounter() attemptCounter {
	return attemptCounter{n: map[string]int{}}
}

// next increments and returns the attempt number for a key.
func (c *attemptCounter) next(key string) int {
	if key == "" {
		// Without a key attempts cannot be tracked; treat every try as the first,
		// since the alternative is dropping alerts that core did not deduplicate.
		return 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[key]++
	return c.n[key]
}

// forget drops a key once it is resolved, so the map does not grow with every
// alert the service has ever seen.
func (c *attemptCounter) forget(key string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.n, key)
}
