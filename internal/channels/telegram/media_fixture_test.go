package telegram

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"slices"
	"testing"
)

type fakeFile struct {
	id          string
	path        string
	declared    int64
	contentType string
	body        []byte
	status      int
	cutOff      bool
	chunked     bool
	infoFails   bool
}

func (f *fakeBotAPI) addFile(file *fakeFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[file.id] = file
}

func (f *fakeBotAPI) fileByID(id string) *fakeFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[id]
}

func (f *fakeBotAPI) fileByPath(path string) *fakeFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = append(f.fetches, path)
	for _, file := range f.files {
		if file.path == path {
			return file
		}
	}
	return nil
}

func (f *fakeBotAPI) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fetches)
}

func (f *fakeBotAPI) serveGetFile(w http.ResponseWriter, body map[string]any) {
	id, _ := body["file_id"].(string)
	file := f.fileByID(id)
	if file == nil || file.infoFails {
		writeFailure(w, http.StatusBadRequest, "Bad Request: wrong file_id", 0)
		return
	}
	info := map[string]any{"file_id": id, "file_path": file.path}
	if file.declared > 0 {
		info["file_size"] = file.declared
	}
	writeOK(w, info)
}

func (f *fakeBotAPI) serveFile(w http.ResponseWriter, path string) {
	file := f.fileByPath(path)
	if file == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if file.status != 0 && file.status != http.StatusOK {
		w.WriteHeader(file.status)
		return
	}
	w.Header().Set("Content-Type", file.contentType)
	switch {
	case file.cutOff:
		w.Header().Set("Content-Length", "20")
		_, _ = w.Write(file.body)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		panic(http.ErrAbortHandler)
	case file.chunked:
		half := len(file.body) / 2
		_, _ = w.Write(file.body[:half])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write(file.body[half:])
	default:
		_, _ = w.Write(file.body)
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.Set(x, y, color.NRGBA{R: uint8(x * 32), G: uint8(y * 32), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
