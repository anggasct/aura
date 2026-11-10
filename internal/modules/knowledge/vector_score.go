package knowledge

import (
	"context"
	"slices"

	"golang.org/x/sync/errgroup"
)

type scoredRow struct {
	score float32
	rowID int64
	entry int
	row   int
}

func betterRow(a, b *scoredRow) bool {
	return a.score > b.score || a.score == b.score && a.rowID < b.rowID
}

type topRows struct {
	limit int
	items []scoredRow
}

func (t *topRows) offer(candidate *scoredRow) {
	if len(t.items) == t.limit && !betterRow(candidate, &t.items[len(t.items)-1]) {
		return
	}
	at, _ := slices.BinarySearchFunc(t.items, *candidate, func(existing, target scoredRow) int {
		switch {
		case betterRow(&existing, &target):
			return -1
		case betterRow(&target, &existing):
			return 1
		default:
			return 0
		}
	})
	if len(t.items) < t.limit {
		t.items = append(t.items, scoredRow{})
	}
	copy(t.items[at+1:], t.items[at:len(t.items)-1])
	t.items[at] = *candidate
}

func dot(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	n := len(a)
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}

type scoreTask struct {
	entry, from, to int
}

func scoreEntries(ctx context.Context, entries []*vectorEntry, query []float32, depth int) ([]scoredRow, error) {
	dimensions := len(query)
	var tasks []scoreTask
	for i, entry := range entries {
		rows := len(entry.rowIDs)
		shard := max((rows+scoreWorkers-1)/scoreWorkers, 1)
		for from := 0; from < rows; from += shard {
			tasks = append(tasks, scoreTask{entry: i, from: from, to: min(from+shard, rows)})
		}
	}
	results := make([]topRows, len(tasks))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(scoreWorkers)
	for n, task := range tasks {
		group.Go(func() error {
			entry := entries[task.entry]
			top := topRows{limit: depth, items: make([]scoredRow, 0, depth)}
			for row := task.from; row < task.to; row++ {
				if (row-task.from)%scoreCancelRows == 0 {
					if err := groupCtx.Err(); err != nil {
						return err
					}
				}
				candidate := scoredRow{score: dot(query, entry.values[row*dimensions:(row+1)*dimensions]), rowID: entry.rowIDs[row], entry: task.entry, row: row}
				top.offer(&candidate)
			}
			results[n] = top
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	merged := topRows{limit: depth, items: make([]scoredRow, 0, depth)}
	for n := range results {
		for i := range results[n].items {
			merged.offer(&results[n].items[i])
		}
	}
	return merged.items, nil
}

type memoryView struct {
	index   *MemoryIndex
	entries []*vectorEntry
}

func (v *memoryView) Len() int { return len(v.entries) }

func (v *memoryView) Search(ctx context.Context, query []float32, depth int) ([]Hit, error) {
	if len(query) != v.index.dimensions || depth <= 0 || depth > SearchDepth {
		return nil, Errorf(ErrorCodeInvalidArgument, "vector query needs %d dimensions and a depth between 1 and %d", v.index.dimensions, SearchDepth)
	}
	rows, err := scoreEntries(ctx, v.entries, query, depth)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	rowIDs := make([]int64, len(rows))
	for i := range rows {
		rowIDs[i] = rows[i].rowID
	}
	chunks, err := v.index.reader.Chunks(ctx, rowIDs)
	if err != nil {
		return nil, err
	}
	byRow := make(map[int64]*VectorChunk, len(chunks))
	for i := range chunks {
		byRow[chunks[i].RowID] = &chunks[i]
	}
	hits := make([]Hit, 0, len(rows))
	for i := range rows {
		entry := v.entries[rows[i].entry]
		chunk := byRow[rows[i].rowID]
		if chunk == nil || chunk.Hit.Collection != entry.collection || chunk.SHA256 != entry.digests[rows[i].row] {
			continue
		}
		hit := chunk.Hit
		hit.Score = float64(rows[i].score)
		hits = append(hits, hit)
	}
	return hits, nil
}
