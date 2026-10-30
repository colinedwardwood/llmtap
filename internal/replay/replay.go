// Package replay defines llmtap's redacted JSONL record/replay format.
package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const FormatVersion = 1

type Entry struct {
	Version           int       `json:"version"`
	Timestamp         time.Time `json:"timestamp"`
	Method            string    `json:"method"`
	Path              string    `json:"path"`
	Provider          string    `json:"provider"`
	Status            int       `json:"status"`
	Stream            bool      `json:"stream"`
	Request           string    `json:"request,omitempty"`
	Response          string    `json:"response,omitempty"`
	RequestTruncated  bool      `json:"request_truncated,omitempty"`
	ResponseTruncated bool      `json:"response_truncated,omitempty"`
}

type Recorder struct {
	path   string
	redact func(string) string
	mu     sync.Mutex
}

func NewRecorder(path string, redact func(string) string) *Recorder {
	if path == "" {
		return nil
	}
	return &Recorder{path: path, redact: redact}
}

func (r *Recorder) Append(entry Entry) error {
	if r == nil {
		return nil
	}
	entry.Version = FormatVersion
	entry.Timestamp = time.Now().UTC()
	if r.redact != nil {
		entry.Path = r.redact(entry.Path)
	}
	entry.Request = r.clean(entry.Request)
	entry.Response = r.clean(entry.Response)
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if mkErr := os.MkdirAll(filepath.Dir(r.path), 0o700); mkErr != nil {
		return mkErr
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (r *Recorder) clean(raw string) string {
	if raw == "" || r.redact == nil {
		return raw
	}
	return r.redact(raw)
}

func Load(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if entry.Version != FormatVersion {
			_ = f.Close()
			return nil, fmt.Errorf("%s:%d: unsupported format version %d", path, line, entry.Version)
		}
		entries = append(entries, entry)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = f.Close()
		return nil, scanErr
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return entries, nil
}

type Difference struct {
	Index int
	Field string
	Left  string
	Right string
}

func Diff(left, right []Entry) []Difference {
	var out []Difference
	n := len(left)
	if len(right) < n {
		n = len(right)
	}
	for i := 0; i < n; i++ {
		if left[i].Status != right[i].Status {
			out = append(out, Difference{i, "status", fmt.Sprint(left[i].Status), fmt.Sprint(right[i].Status)})
		}
		l, r := canonical(left[i].Response), canonical(right[i].Response)
		if !bytes.Equal(l, r) {
			out = append(out, Difference{i, "response", string(l), string(r)})
		}
	}
	if len(left) != len(right) {
		out = append(out, Difference{n, "entry_count", fmt.Sprint(len(left)), fmt.Sprint(len(right))})
	}
	return out
}

func canonical(raw string) []byte {
	if raw == "" {
		return nil
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return bytes.TrimSpace([]byte(raw))
	}
	out, err := json.Marshal(value)
	if err != nil {
		return bytes.TrimSpace([]byte(raw))
	}
	return out
}
