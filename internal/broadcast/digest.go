package broadcast

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"
)

const digestContentOverhead = 128

const digestProducer = "system:digest"

type digestPart struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

type digestDocument struct {
	Window      string       `json:"window"`
	Destination string       `json:"destination"`
	Priority    string       `json:"priority"`
	Count       int          `json:"count"`
	Items       []digestPart `json:"items"`
}

func groupKey(item *Item) string {
	return item.DestinationAlias + "|" + item.Priority + "|" + item.NotBefore.UTC().Format(time.RFC3339Nano)
}

func GroupDigests(items []Item, maxItems int, maxBytes int64) [][]Item {
	eligible := make([]Item, 0, len(items))
	for i := range items {
		if items[i].Priority == PriorityUrgent {
			continue
		}
		if items[i].ID == "" {
			continue
		}
		eligible = append(eligible, items[i])
	}
	slices.SortFunc(eligible, func(a, b Item) int {
		if a.DestinationAlias != b.DestinationAlias {
			return strings.Compare(a.DestinationAlias, b.DestinationAlias)
		}
		if a.Priority != b.Priority {
			return strings.Compare(a.Priority, b.Priority)
		}
		if !a.NotBefore.Equal(b.NotBefore) {
			return a.NotBefore.Compare(b.NotBefore)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Compare(b.CreatedAt)
		}
		return strings.Compare(a.ID, b.ID)
	})
	var groups [][]Item
	current := make([]Item, 0, len(eligible))
	var currentKey string
	var currentBytes int64
	flush := func() {
		if len(current) > 0 {
			groups = append(groups, current)
			current = make([]Item, 0, len(eligible))
			currentBytes = 0
		}
	}
	for i := range eligible {
		key := groupKey(&eligible[i])
		size := int64(len(eligible[i].ContentJSON)) + digestContentOverhead
		if len(current) > 0 && (key != currentKey || len(current) >= maxItems || currentBytes+size > maxBytes) {
			flush()
		}
		if len(current) == 0 {
			currentKey = key
		}
		current = append(current, eligible[i])
		currentBytes += size
	}
	flush()
	return groups
}

func renderDigestContent(window time.Time, destination, priority string, group []Item) (string, error) {
	document := digestDocument{
		Window:      window.UTC().Format(time.RFC3339Nano),
		Destination: destination,
		Priority:    priority,
		Count:       len(group),
		Items:       make([]digestPart, 0, len(group)),
	}
	for i := range group {
		document.Items = append(document.Items, digestPart{ID: group[i].ID, Content: json.RawMessage(group[i].ContentJSON)})
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return "", Errorf(ErrorCodeInvalidArgument, "digest content is not serializable")
	}
	return string(raw), nil
}

func digestParentKey(window time.Time, destination, priority string, index int) string {
	return "digest:" + window.UTC().Format(time.RFC3339Nano) + ":" + destination + ":" + priority + ":" + strconv.Itoa(index)
}

func digestParentID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "bcst_dg_" + hex.EncodeToString(sum[:])[:16]
}

func (b *Broadcaster) CreateDigest(ctx context.Context, group []Item, index int) (Item, bool, error) {
	if ctx == nil {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if len(group) == 0 {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "digest group must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return Item{}, false, err
	}
	first := &group[0]
	for i := 1; i < len(group); i++ {
		if groupKey(&group[i]) != groupKey(first) {
			return Item{}, false, Errorf(ErrorCodeInvalidArgument, "digest group mixes windows or destinations")
		}
	}
	if first.Priority == PriorityUrgent {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "urgent items are never digested")
	}
	content, err := renderDigestContent(first.NotBefore, first.DestinationAlias, first.Priority, group)
	if err != nil {
		return Item{}, false, err
	}
	if int64(len(content)) > b.maxDigestBytes {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "digest content exceeds the byte bound")
	}
	now := time.Now().UTC()
	key := digestParentKey(first.NotBefore, first.DestinationAlias, first.Priority, index)
	childIDs := make([]string, 0, len(group))
	for i := range group {
		childIDs = append(childIDs, group[i].ID)
	}
	record, replayed, err := b.items.CreateDigest(ctx, &ItemRecord{
		ID:               digestParentID(key),
		Producer:         digestProducer,
		IdempotencyKey:   key,
		ContentDigest:    digestContent(content),
		Priority:         first.Priority,
		DestinationAlias: first.DestinationAlias,
		ContentJSON:      content,
		State:            StateScheduled,
		NotBefore:        first.NotBefore,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, childIDs)
	if err != nil {
		return Item{}, false, err
	}
	return Item{
		ID:               record.ID,
		Producer:         record.Producer,
		IdempotencyKey:   record.IdempotencyKey,
		ContentDigest:    record.ContentDigest,
		Priority:         record.Priority,
		DestinationAlias: record.DestinationAlias,
		ContentJSON:      record.ContentJSON,
		State:            record.State,
		NotBefore:        record.NotBefore,
		CreatedAt:        record.CreatedAt,
	}, replayed, nil
}
