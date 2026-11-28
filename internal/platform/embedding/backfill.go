package embedding

import "sync"

type BackfillCursor struct {
	mu        sync.Mutex
	createdAt string
	id        string
	exhausted bool
}

func (c *BackfillCursor) Resume() (createdAt, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exhausted {
		c.createdAt, c.id, c.exhausted = "", "", false
	}
	return c.createdAt, c.id
}

func (c *BackfillCursor) Advance(createdAt, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createdAt, c.id = createdAt, id
}

func (c *BackfillCursor) MarkExhausted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exhausted = true
}
