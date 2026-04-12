package trainer

import (
	"path/filepath"
	"testing"
	"time"
)

func TestJobStoreSaveLoad(t *testing.T) {
	dir := t.TempDir()
	store := &jobStore{
		path:       filepath.Join(dir, "jobs.json"),
		maxEntries: 50,
		maxAge:     30 * 24 * time.Hour,
	}
	jobs := map[string]JobRecord{
		"cmd-1": {
			CommandID: "cmd-1",
			Status:    "success",
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := store.Save(jobs); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 job, got %d", len(loaded))
	}
}

func TestJobStorePrunesOldJobs(t *testing.T) {
	now := time.Now().UTC()
	store := &jobStore{
		maxEntries: 10,
		maxAge:     24 * time.Hour,
	}
	jobs := []JobRecord{
		{
			CommandID:   "old",
			Status:      "success",
			StartedAt:   now.Add(-72 * time.Hour).Format(time.RFC3339),
			CompletedAt: now.Add(-72 * time.Hour).Format(time.RFC3339),
		},
		{
			CommandID: "active",
			Status:    "in_progress",
			StartedAt: now.Format(time.RFC3339),
		},
	}
	store.prune(&jobs)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job after prune, got %d", len(jobs))
	}
	if jobs[0].CommandID != "active" {
		t.Fatalf("expected active job to remain, got %s", jobs[0].CommandID)
	}
}
