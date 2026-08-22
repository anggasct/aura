package filesystem

type fileResult struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

type directoryEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

type directoryResult struct {
	Entries   []directoryEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
}
