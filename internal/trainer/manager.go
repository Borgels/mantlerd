package trainer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

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
	CommandID string
	TrainerID string
	Detail    string
	Progress  TrainingProgress
}

type Manager struct {
	mu      sync.Mutex
	drivers map[string]Driver

	trainers map[string]TrainerRecord
	jobs     map[string]JobRecord
	cancels  map[string]context.CancelFunc
}

func NewManager() *Manager {
	return &Manager{
		drivers:  NewDriverRegistry(),
		trainers: make(map[string]TrainerRecord),
		jobs:     make(map[string]JobRecord),
		cancels:  make(map[string]context.CancelFunc),
	}
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
		CommandID: req.CommandID,
		TrainerID: record.ID,
		Detail:    fmt.Sprintf("Training %s", req.BaseModel),
	}
	record.Status = types.TrainerTraining
	record.Detail = fmt.Sprintf("Training %s", req.BaseModel)
	m.trainers[record.ID] = record
	m.mu.Unlock()

	result, runErr := driver.StartTraining(jobCtx, req, func(progress TrainingProgress) {
		m.mu.Lock()
		job := m.jobs[req.CommandID]
		job.Progress = progress
		m.jobs[req.CommandID] = job
		m.mu.Unlock()
		if emitProgress != nil {
			emitProgress(progress)
		}
	})

	m.mu.Lock()
	delete(m.cancels, req.CommandID)
	delete(m.jobs, req.CommandID)
	updated := m.trainers[record.ID]
	if runErr != nil {
		updated.Status = types.TrainerFailed
		updated.Detail = runErr.Error()
		m.trainers[record.ID] = updated
		m.refreshTrainerStatusLocked(record.ID)
		m.mu.Unlock()
		return TrainingResult{}, runErr
	}
	updated.Status = types.TrainerReady
	updated.Detail = result.Detail
	updated.Version = nonEmpty(updated.Version, "unknown")
	m.trainers[record.ID] = updated
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
	driver := m.drivers[strings.ToLower(string(trainerRecord.Type))]
	if driver == nil {
		return nil
	}
	if err := driver.StopTraining(ctx, commandID, saveCheckpoint); err != nil {
		return err
	}
	return nil
}

func (m *Manager) GetJobStatus(commandID string) (TrainingProgress, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[commandID]
	if !ok {
		return TrainingProgress{}, false
	}
	return job.Progress, true
}

func (m *Manager) ExportCheckpoint(ctx context.Context, trainingCommandID string, trainerType string, formats []string) (ExportResult, error) {
	m.mu.Lock()
	job, ok := m.jobs[trainingCommandID]
	m.mu.Unlock()
	resolvedTrainerType := strings.TrimSpace(strings.ToLower(trainerType))
	if ok {
		m.mu.Lock()
		trainerRecord, hasTrainer := m.trainers[job.TrainerID]
		m.mu.Unlock()
		if !hasTrainer {
			return ExportResult{}, fmt.Errorf("trainer not found for training command: %s", trainingCommandID)
		}
		resolvedTrainerType = strings.ToLower(strings.TrimSpace(string(trainerRecord.Type)))
	}
	if resolvedTrainerType == "" {
		return ExportResult{}, fmt.Errorf("training command not found: %s", trainingCommandID)
	}
	driver := m.drivers[resolvedTrainerType]
	if driver == nil {
		return ExportResult{}, fmt.Errorf("unsupported trainer: %s", resolvedTrainerType)
	}
	return driver.ExportCheckpoint(ctx, trainingCommandID, formats)
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
	return len(m.jobs) > 0
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
