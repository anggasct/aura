package telegram

import (
	"container/list"
	"sync"
)

const maxTrackedPrivateChats = 1024

type latestEntry struct {
	chatID    int64
	fromID    int64
	messageID int64
}

type latestInbound struct {
	mu      sync.Mutex
	entries map[int64]*list.Element
	order   *list.List
}

func (t *latestInbound) record(chatID, fromID, messageID int64) {
	if chatID == 0 || messageID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[int64]*list.Element)
		t.order = list.New()
	}
	if elem, ok := t.entries[chatID]; ok {
		if entry, valid := elem.Value.(*latestEntry); valid && messageID > entry.messageID {
			entry.messageID = messageID
			entry.fromID = fromID
		}
		t.order.MoveToBack(elem)
		return
	}
	if t.order.Len() >= maxTrackedPrivateChats {
		oldest := t.order.Front()
		t.order.Remove(oldest)
		if entry, valid := oldest.Value.(*latestEntry); valid {
			delete(t.entries, entry.chatID)
		}
	}
	elem := t.order.PushBack(&latestEntry{chatID: chatID, fromID: fromID, messageID: messageID})
	t.entries[chatID] = elem
}

func (t *latestInbound) hasNewer(chatID, fromID, triggerID int64) bool {
	if chatID == 0 || triggerID <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	elem, ok := t.entries[chatID]
	if !ok {
		return false
	}
	entry, valid := elem.Value.(*latestEntry)
	if !valid {
		return false
	}
	if fromID != 0 && entry.fromID != 0 && entry.fromID != fromID {
		return false
	}
	return entry.messageID > triggerID
}
