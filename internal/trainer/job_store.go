package trainer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type jobStore struct {
	path       string
	maxEntries int
	maxAge     time.Duration
}

type storedJobs struct {
	Jobs []JobRecord `json:"jobs"`
}

func newJobStore(dataDir string) *jobStore {
	return &jobStore{
		path:       filepath.Join(dataDir, "jobs.json"),
		maxEntries: 50,
		maxAge:     30 * 24 * time.Hour,
	}
}

func (s *jobStore) Load() (map[string]JobRecord, error) {
	bytes, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]JobRecord{}, nil
		}
		return nil, fmt.Errorf("read job store: %w", err)
	}
	if len(bytes) == 0 {
		return map[string]JobRecord{}, nil
	}
	var payload storedJobs
	if err := json.Unmarshal(bytes, &payload); err != nil {
		return nil, fmt.Errorf("parse job store: %w", err)
	}
	result := make(map[string]JobRecord, len(payload.Jobs))
	for _, job := range payload.Jobs {
		if strings.TrimSpace(job.CommandID) == "" {
			continue
		}
		result[job.CommandID] = job
	}
	return result, nil
}

func (s *jobStore) Save(jobs map[string]JobRecord) error {
	copied := make([]JobRecord, 0, len(jobs))
	for _, job := range jobs {
		copied = append(copied, job)
	}
	s.prune(&copied)
	sort.Slice(copied, func(i, j int) bool {
		iTime := parseTimestamp(copied[i].StartedAt)
		jTime := parseTimestamp(copied[j].StartedAt)
		return iTime.After(jTime)
	})
	payload := storedJobs{Jobs: copied}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode job store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create job store dir: %w", err)
	}
	if err := os.WriteFile(s.path, raw, 0o644); err != nil {
		return fmt.Errorf("write job store: %w", err)
	}
	return nil
}

func (s *jobStore) prune(records *[]JobRecord) {
	cutoff := time.Now().Add(-s.maxAge)
	filtered := make([]JobRecord, 0, len(*records))
	for _, job := range *records {
		if isActiveJobStatus(job.Status) {
			filtered = append(filtered, job)
			continue
		}
		completed := parseTimestamp(job.CompletedAt)
		if completed.IsZero() {
			completed = parseTimestamp(job.StartedAt)
		}
		if completed.IsZero() || completed.After(cutoff) {
			filtered = append(filtered, job)
		}
	}
	if len(filtered) > s.maxEntries {
		sort.Slice(filtered, func(i, j int) bool {
			return parseTimestamp(filtered[i].StartedAt).After(parseTimestamp(filtered[j].StartedAt))
		})
		filtered = filtered[:s.maxEntries]
	}
	*records = filtered
}

func parseTimestamp(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}
