package trainer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

type TrainerRecord struct {
	ID      string
	Type    string
	Name    string
	Status  types.TrainerStatus
	Version string
	Detail  string
}

type JobRecord struct {
	CommandID   string           `json:"commandId"`
	TrainerID   string           `json:"trainerId"`
	TrainerType string           `json:"trainerType"`
	Status      string           `json:"status"`
	Detail      string           `json:"detail,omitempty"`
	Progress    TrainingProgress `json:"progress,omitempty"`
	Artifacts   []ExportArtifact `json:"artifacts,omitempty"`
	Error       string           `json:"error,omitempty"`
	StartedAt   string           `json:"startedAt,omitempty"`
	CompletedAt string           `json:"completedAt,omitempty"`
}

type Manager struct {
	mu      sync.Mutex
	drivers map[string]Driver

	trainers map[string]TrainerRecord
	jobs     map[string]JobRecord
	cancels  map[string]context.CancelFunc
	store    *jobStore
}

func NewManager() *Manager {
	runner := newContainerRunner()
	manager := &Manager{
		drivers:  NewDriverRegistry(),
		trainers: make(map[string]TrainerRecord),
		jobs:     map[string]JobRecord{},
		cancels:  make(map[string]context.CancelFunc),
		store:    newJobStore(runner.dataDir),
	}
	if loaded, err := manager.store.Load(); err == nil {
		manager.jobs = loaded
	}
	return manager
}

func (m *Manager) Install(ctx context.Context, trainerType string) (string, error) {
	driver, record, err := m.resolveTrainer(trainerType)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	record.Status = types.TrainerInstalling
	record.Detail = "Installing trainer"
	m.trainers[record.ID] = record
	m.mu.Unlock()

	version, installErr := driver.Install(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.trainers[record.ID]
	if installErr != nil {
		current.Status = types.TrainerFailed
		current.Detail = installErr.Error()
		m.trainers[record.ID] = current
		return "", installErr
	}
	current.Status = types.TrainerReady
	current.Version = version
	current.Detail = "Trainer ready"
	m.trainers[record.ID] = current
	return version, nil
}

func (m *Manager) Uninstall(ctx context.Context, trainerType string) error {
	driver, record, err := m.resolveTrainer(trainerType)
	if err != nil {
		return err
	}
	if uninstallErr := driver.Uninstall(ctx); uninstallErr != nil {
		m.mu.Lock()
		current := m.trainers[record.ID]
		current.Status = types.TrainerFailed
		current.Detail = uninstallErr.Error()
		m.trainers[record.ID] = current
		m.mu.Unlock()
		return uninstallErr
	}
	m.mu.Lock()
	current := m.trainers[record.ID]
	current.Status = types.TrainerNotInstalled
	current.Detail = "Trainer not installed"
	m.trainers[record.ID] = current
	m.mu.Unlock()
	return nil
}

func (m *Manager) StartTraining(
	ctx context.Context,
	req TrainingRequest,
	emitProgress func(progress TrainingProgress),
) (TrainingResult, error) {
	driver, record, err := m.resolveTrainer(req.TrainerType)
	if err != nil {
		return TrainingResult{}, err
	}
	if record.Status != types.TrainerReady && record.Status != types.TrainerTraining {
		return TrainingResult{}, fmt.Errorf("trainer %s is not ready (current status: %s)", record.Type, record.Status)
	}
	jobCtx, cancel := context.WithCancel(ctx)

	m.mu.Lock()
	m.cancels[req.CommandID] = cancel
	m.jobs[req.CommandID] = JobRecord{
		CommandID:   req.CommandID,
		TrainerID:   record.ID,
		TrainerType: record.Type,
		Status:      "in_progress",
		Detail:      fmt.Sprintf("Training %s", req.BaseModel),
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	record.Status = types.TrainerTraining
	record.Detail = fmt.Sprintf("Training %s", req.BaseModel)
	m.trainers[record.ID] = record
	m.persistJobsLocked()
	m.mu.Unlock()

	result, runErr := driver.StartTraining(jobCtx, req, func(progress TrainingProgress) {
		m.mu.Lock()
		job := m.jobs[req.CommandID]
		job.Progress = progress
		job.Status = firstNonEmpty(progress.Status, "in_progress")
		job.Detail = firstNonEmpty(progress.Detail, job.Detail)
		m.jobs[req.CommandID] = job
		m.persistJobsLocked()
		m.mu.Unlock()
		if emitProgress != nil {
			emitProgress(progress)
		}
	})

	m.mu.Lock()
	delete(m.cancels, req.CommandID)
	updated := m.trainers[record.ID]
	job := m.jobs[req.CommandID]
	if runErr != nil {
		job.Status = "failed"
		job.Error = runErr.Error()
		job.Detail = runErr.Error()
		job.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		m.jobs[req.CommandID] = job
		updated.Status = types.TrainerFailed
		updated.Detail = runErr.Error()
		m.trainers[record.ID] = updated
		m.persistJobsLocked()
		m.refreshTrainerStatusLocked(record.ID)
		m.mu.Unlock()
		return TrainingResult{}, runErr
	}
	job.Status = "success"
	job.Detail = result.Detail
	job.Artifacts = result.Artifacts
	job.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	m.jobs[req.CommandID] = job
	updated.Status = types.TrainerReady
	updated.Detail = result.Detail
	updated.Version = nonEmpty(updated.Version, "unknown")
	m.trainers[record.ID] = updated
	m.persistJobsLocked()
	m.refreshTrainerStatusLocked(record.ID)
	m.mu.Unlock()
	return result, nil
}

func (m *Manager) StopTraining(ctx context.Context, commandID string, saveCheckpoint bool) error {
	m.mu.Lock()
	cancel, hasCancel := m.cancels[commandID]
	job, hasJob := m.jobs[commandID]
	m.mu.Unlock()
	if !hasCancel && !hasJob {
		return fmt.Errorf("unknown training command: %s", commandID)
	}
	if cancel != nil {
		cancel()
	}
	if job.TrainerID == "" {
		return nil
	}
	m.mu.Lock()
	trainerRecord, ok := m.trainers[job.TrainerID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	driver := m.drivers[strings.ToLower(strings.TrimSpace(trainerRecord.Type))]
	if driver == nil {
		return nil
	}
	if err := driver.StopTraining(ctx, commandID, saveCheckpoint); err != nil {
		return err
	}
	m.mu.Lock()
	job = m.jobs[commandID]
	job.Status = "failed"
	job.Detail = "training stopped"
	job.Error = "stopped by user"
	job.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	m.jobs[commandID] = job
	m.persistJobsLocked()
	m.mu.Unlock()
	return nil
}

func (m *Manager) GetJobStatus(commandID string) (TrainingProgress, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[commandID]
	if !ok {
		return TrainingProgress{}, false
	}
	progress := job.Progress
	progress.Detail = firstNonEmpty(progress.Detail, job.Detail)
	progress.Status = firstNonEmpty(progress.Status, job.Status)
	return progress, true
}

func (m *Manager) ExportCheckpoint(ctx context.Context, trainingCommandID string, trainerType string, formats []string) (ExportResult, error) {
	m.mu.Lock()
	job, ok := m.jobs[trainingCommandID]
	m.mu.Unlock()
	resolvedTrainerType := strings.TrimSpace(strings.ToLower(trainerType))
	if ok && strings.TrimSpace(job.TrainerType) != "" {
		resolvedTrainerType = strings.ToLower(strings.TrimSpace(job.TrainerType))
	}
	if resolvedTrainerType == "" {
		return ExportResult{}, fmt.Errorf("training command not found: %s", trainingCommandID)
	}
	driver := m.drivers[resolvedTrainerType]
	if driver == nil {
		return ExportResult{}, fmt.Errorf("unsupported trainer: %s", resolvedTrainerType)
	}
	exportResult, err := driver.ExportCheckpoint(ctx, trainingCommandID, formats)
	if err != nil {
		return ExportResult{}, err
	}
	m.mu.Lock()
	if existing, exists := m.jobs[trainingCommandID]; exists {
		existing.Artifacts = exportResult.Exports
		m.jobs[trainingCommandID] = existing
		m.persistJobsLocked()
	}
	m.mu.Unlock()
	return exportResult, nil
}

func (m *Manager) Jobs(commandID string) []JobRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	results := make([]JobRecord, 0)
	if commandID != "" {
		if job, ok := m.jobs[commandID]; ok {
			results = append(results, job)
		}
		return results
	}
	for _, job := range m.jobs {
		results = append(results, job)
	}
	sort.Slice(results, func(i, j int) bool {
		return parseTimestamp(results[i].StartedAt).After(parseTimestamp(results[j].StartedAt))
	})
	return results
}

func (m *Manager) InstalledTrainers() []types.InstalledTrainer {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]types.InstalledTrainer, 0, len(m.trainers))
	for _, trainer := range m.trainers {
		trainer = m.derivedTrainerStatusLocked(trainer)
		result = append(result, types.InstalledTrainer{
			ID:      trainer.ID,
			Type:    types.TrainerType(trainer.Type),
			Name:    trainer.Name,
			Status:  trainer.Status,
			Version: trainer.Version,
			Detail:  trainer.Detail,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Type == result[j].Type {
			return result[i].ID < result[j].ID
		}
		return result[i].Type < result[j].Type
	})
	return result
}

func (m *Manager) HasActiveJobs() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		if isActiveJobStatus(job.Status) {
			return true
		}
	}
	return false
}

func (m *Manager) resolveTrainer(trainerType string) (Driver, TrainerRecord, error) {
	normalized := strings.ToLower(strings.TrimSpace(trainerType))
	if normalized == "" {
		normalized = "unsloth"
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	driver, ok := m.drivers[normalized]
	if !ok {
		return nil, TrainerRecord{}, fmt.Errorf("unsupported trainer: %s", trainerType)
	}

	recordID := "trainer-" + normalized
	record, hasRecord := m.trainers[recordID]
	if !hasRecord {
		record = TrainerRecord{
			ID:      recordID,
			Type:    normalized,
			Name:    displayNameFromType(normalized),
			Status:  types.TrainerNotInstalled,
			Detail:  "Trainer not installed",
			Version: "",
		}
		m.trainers[recordID] = record
	}
	return driver, record, nil
}

func displayNameFromType(trainerType string) string {
	switch trainerType {
	case "unsloth":
		return "Unsloth"
	case "axolotl":
		return "Axolotl"
	case "trl":
		return "TRL"
	case "llamafactory":
		return "LLaMA-Factory"
	default:
		return strings.TrimSpace(trainerType)
	}
}

func nonEmpty(value string, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func (m *Manager) refreshTrainerStatusLocked(trainerID string) {
	trainer, ok := m.trainers[trainerID]
	if !ok {
		return
	}
	m.trainers[trainerID] = m.derivedTrainerStatusLocked(trainer)
}

func (m *Manager) derivedTrainerStatusLocked(trainer TrainerRecord) TrainerRecord {
	for _, job := range m.jobs {
		if job.TrainerID != trainer.ID {
			continue
		}
		if !isActiveJobStatus(job.Status) {
			continue
		}
		trainer.Status = types.TrainerTraining
		if strings.TrimSpace(job.Detail) != "" {
			trainer.Detail = job.Detail
		} else {
			trainer.Detail = "Training in progress"
		}
		return trainer
	}
	if trainer.Status == types.TrainerTraining {
		trainer.Status = types.TrainerReady
		if strings.TrimSpace(trainer.Detail) == "" || strings.EqualFold(strings.TrimSpace(trainer.Detail), "training in progress") {
			trainer.Detail = "Trainer ready"
		}
	}
	return trainer
}

func isActiveJobStatus(status string) bool {
	normalized := strings.TrimSpace(strings.ToLower(status))
	return normalized == "pending" || normalized == "in_progress" || normalized == "running"
}

func (m *Manager) persistJobsLocked() {
	if m.store == nil {
		return
	}
	if err := m.store.Save(m.jobs); err != nil {
		return
	}
}
