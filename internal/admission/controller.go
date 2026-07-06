package admission

import "sync"

// Controller tracks bounded in-flight work by scope. It is intentionally small:
// callers own policy (which scopes to use and which limit applies).
type Controller struct {
	mu       sync.Mutex
	inflight map[string]int
}

func NewController() *Controller {
	return &Controller{inflight: map[string]int{}}
}

func (c *Controller) TryAcquire(scope string, limit int) (func(), bool) {
	if scope == "" || limit <= 0 {
		return func() {}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[scope] >= limit {
		return nil, false
	}
	c.inflight[scope]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.release(scope)
		})
	}, true
}

func (c *Controller) release(scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[scope] <= 1 {
		delete(c.inflight, scope)
		return
	}
	c.inflight[scope]--
}

func (c *Controller) Snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.inflight))
	for k, v := range c.inflight {
		out[k] = v
	}
	return out
}
