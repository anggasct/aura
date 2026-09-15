package sync

import (
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
	Counters    Counters
	LastResult  string
	LastLatency time.Duration
	LastEntries int
	LastBytes   int64
}

func (m *Metrics) Record(observation Observation) {
	m.Counters.Runs++
	if observation.Conflict {
		m.Counters.Conflicts++
	}
	if observation.Unknown {
		m.Counters.Unknowns++
	}
	m.LastResult = observation.Result
	m.LastLatency = observation.Latency
	m.LastEntries = observation.EntryCount
	m.LastBytes = observation.ByteCount
}
