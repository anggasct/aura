package knowledge

import "context"

type loadOutcome struct {
	entry    *vectorEntry
	reject   *rejection
	reserved int64
	cancel   bool
}

func (m *MemoryIndex) load(ctx context.Context, request loadRequest) {
	outcome := m.build(ctx, request)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserved -= outcome.reserved
	delete(m.pending, request.collection)
	if len(m.pending) == 0 {
		close(m.idle)
	}
	switch {
	case outcome.cancel:
	case outcome.entry != nil:
		m.dropLocked(request.collection)
		m.entries[request.collection] = outcome.entry
		m.used += outcome.entry.bytes
		delete(m.rejected, request.collection)
	case outcome.reject != nil:
		m.rejected[request.collection] = *outcome.reject
	}
}

func (m *MemoryIndex) failed(ctx context.Context, request loadRequest) loadOutcome {
	if ctx.Err() != nil {
		return loadOutcome{cancel: true}
	}
	return loadOutcome{reject: &rejection{generation: request.generation, until: m.now().Add(loadRetryInterval)}}
}

func (m *MemoryIndex) settled(request loadRequest) loadOutcome {
	return loadOutcome{reject: &rejection{generation: request.generation}}
}

func (m *MemoryIndex) build(ctx context.Context, request loadRequest) loadOutcome {
	state, err := m.loader.EmbeddingState(ctx, request.collection, m.fingerprint)
	if err != nil {
		return m.failed(ctx, request)
	}
	if !state.Exists || state.Fingerprint != m.fingerprint || state.Chunks == 0 || state.Covered != state.Chunks {
		return m.settled(request)
	}
	need := VectorBytes(state.Chunks, m.dimensions)
	m.mu.Lock()
	fits := m.used+m.reserved+need <= m.limit
	if fits {
		m.reserved += need
	}
	m.mu.Unlock()
	if !fits {
		return loadOutcome{reject: &rejection{generation: request.generation, need: need}}
	}
	entry, outcome := m.readEntry(ctx, request, &state)
	outcome.reserved = need
	if entry == nil {
		return outcome
	}
	versions, err := m.loader.Versions(ctx, []string{request.collection})
	if err != nil {
		failure := m.failed(ctx, request)
		failure.reserved = need
		return failure
	}
	if len(versions) != 1 || versions[0].Generation != request.generation || versions[0].Fingerprint != m.fingerprint {
		return loadOutcome{reserved: need}
	}
	return loadOutcome{entry: entry, reserved: need}
}

func (m *MemoryIndex) readEntry(ctx context.Context, request loadRequest, state *EmbeddingState) (*vectorEntry, loadOutcome) {
	entry := &vectorEntry{
		collection: request.collection, generation: request.generation,
		rowIDs:  make([]int64, 0, state.Chunks),
		digests: make([]string, 0, state.Chunks),
		values:  make([]float32, 0, state.Chunks*m.dimensions),
		bytes:   VectorBytes(state.Chunks, m.dimensions),
	}
	var after int64
	for {
		page, err := m.loader.Page(ctx, request.collection, m.fingerprint, m.dimensions, after, loadPageSize)
		if err != nil {
			return nil, m.failed(ctx, request)
		}
		rows := len(page.RowIDs)
		if rows == 0 {
			break
		}
		if len(page.Digests) != rows || len(page.Values) != rows*m.dimensions || len(entry.rowIDs)+rows > state.Chunks {
			return nil, m.settled(request)
		}
		entry.rowIDs = append(entry.rowIDs, page.RowIDs...)
		entry.digests = append(entry.digests, page.Digests...)
		entry.values = append(entry.values, page.Values...)
		if rows < loadPageSize {
			break
		}
		after = page.RowIDs[rows-1]
	}
	if len(entry.rowIDs) != state.Chunks {
		return nil, m.settled(request)
	}
	return entry, loadOutcome{}
}
