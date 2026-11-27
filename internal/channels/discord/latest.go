package discord

import (
	"container/list"
	"strconv"
	"sync"
)

const maxTrackedDMs = 1024

type latestEntry struct {
	channelID string
	authorID  string
	messageID string
}

type latestInbound struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
}

func (t *latestInbound) record(channelID, authorID, messageID string) {
	if channelID == "" || messageID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*list.Element)
		t.order = list.New()
	}
	if elem, ok := t.entries[channelID]; ok {
		if entry, valid := elem.Value.(*latestEntry); valid && messageIDAfter(messageID, entry.messageID) {
			entry.messageID = messageID
			entry.authorID = authorID
		}
		t.order.MoveToBack(elem)
		return
	}
	if t.order.Len() >= maxTrackedDMs {
		oldest := t.order.Front()
		t.order.Remove(oldest)
		if entry, valid := oldest.Value.(*latestEntry); valid {
			delete(t.entries, entry.channelID)
		}
	}
	elem := t.order.PushBack(&latestEntry{channelID: channelID, authorID: authorID, messageID: messageID})
	t.entries[channelID] = elem
}

func (t *latestInbound) hasNewer(channelID, authorID, triggerID string) bool {
	if channelID == "" || triggerID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	elem, ok := t.entries[channelID]
	if !ok {
		return false
	}
	entry, valid := elem.Value.(*latestEntry)
	if !valid {
		return false
	}
	if authorID != "" && entry.authorID != "" && entry.authorID != authorID {
		return false
	}
	return messageIDAfter(entry.messageID, triggerID)
}

func messageIDAfter(candidate, baseline string) bool {
	if candidate == "" || baseline == "" || candidate == baseline {
		return false
	}
	c, cErr := strconv.ParseUint(candidate, 10, 64)
	b, bErr := strconv.ParseUint(baseline, 10, 64)
	if cErr == nil && bErr == nil {
		return c > b
	}
	return true
}
