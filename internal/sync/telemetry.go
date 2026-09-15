package sync

import (
	"sync"
	"time"
)

type Observation struct {
	Result        string
	Conflict      bool
	Unknown       bool
	Latency       time.Duration
	EntryCount    int
	ByteCount     int64
	Findings      int
	Promoted      int
	SkippedSkills int
}

type Counters struct {
	Runs      uint64
	Conflicts uint64
	Unknowns  uint64
}

type Metrics struct {
	mu          sync.Mutex
	Counters    Counters
	LastResult  string
	LastLatency time.Duration
	LastEntries int
	LastBytes   int64
}

func normalizeSyncResult(raw string) string {
	switch raw {
	case string(AdvanceUpToDate),
		string(AdvanceConflict),
		string(WorkerUnknown),
		string(AdvanceFastForward),
		"pushed",
		"fast_forwarded":
		return raw
	case "":
		return string(WorkerUnknown)
	default:
		return string(WorkerUnknown)
	}
}

func (m *Metrics) Record(observation Observation) {
	normalized := normalizeSyncResult(observation.Result)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Counters.Runs++
	if observation.Conflict {
		m.Counters.Conflicts++
	}
	if observation.Unknown {
		m.Counters.Unknowns++
	}
	m.LastResult = normalized
	m.LastLatency = observation.Latency
	m.LastEntries = observation.EntryCount
	m.LastBytes = observation.ByteCount
}

func (m *Metrics) Snapshot() Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Metrics{
		Counters:    m.Counters,
		LastResult:  m.LastResult,
		LastLatency: m.LastLatency,
		LastEntries: m.LastEntries,
		LastBytes:   m.LastBytes,
	}
}
