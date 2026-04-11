package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Borgels/mantlerd/internal/client"
	"github.com/Borgels/mantlerd/internal/commands"
	"github.com/Borgels/mantlerd/internal/config"
	"github.com/Borgels/mantlerd/internal/discovery"
	runtimeagent "github.com/Borgels/mantlerd/internal/runtime"
	"github.com/Borgels/mantlerd/internal/trainer"
	"github.com/Borgels/mantlerd/internal/types"
	"github.com/spf13/cobra"
)

const (
	defaultLightCommandConcurrency = 8
	heavyCommandQueueSize          = 64
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the mantler daemon agent",
	Long: `Start the mantler daemon agent which performs periodic check-ins
to the Mantler server, reports machine metadata, and executes commands.`,
	Run: runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
}

func runStart(cmd *cobra.Command, args []string) {
	// Load configuration
	cfg := loadConfig(cmd)

	// Create API client
	cl, err := client.New(cfg.ServerURL, cfg.Token, cfg.Insecure)
	if err != nil {
		log.Fatalf("create api client: %v", err)
	}

	outcomes := &outcomeBuffer{}

	// Create runtime manager and executor
	runtimeManager := runtimeagent.NewManager()
	trainerManager := trainer.NewManager()
	runtimeManager.SetOutcomeReporter(outcomes.Add)
	executor := commands.NewExecutor(runtimeManager, trainerManager, cfg, func(payload types.AckRequest) {
		sendInProgressAck(cl, payload)
	}, outcomes.Add)

	// Set up signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dispatcher := newCommandDispatcher(ctx, executor, cl, defaultLightCommandConcurrency)

	// Run initial check-in
	startedAt := time.Now()
	activeOperations := runCheckIn(ctx, cfg, cl, runtimeManager, trainerManager, executor, outcomes, dispatcher, startedAt)
	timer := time.NewTimer(nextCheckinInterval(cfg.Interval, activeOperations))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down agent...")
			return
		case <-timer.C:
			activeOperations = runCheckIn(ctx, cfg, cl, runtimeManager, trainerManager, executor, outcomes, dispatcher, startedAt)
			timer.Reset(nextCheckinInterval(cfg.Interval, activeOperations))
		}
	}
}

func loadConfig(cmd *cobra.Command) config.Config {
	configPath := cfgFile
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}

	fileCfg := config.Config{}
	loadedCfg, err := config.Load(configPath)
	if err == nil {
		fileCfg = loadedCfg
	} else if !os.IsNotExist(err) {
		log.Fatalf("load config: %v", err)
	}

	flagsCfg := config.Config{}
	if cmd.Flags().Changed("server") {
		flagsCfg.ServerURL = serverURL
	}
	if cmd.Flags().Changed("token") {
		flagsCfg.Token = token
	}
	if cmd.Flags().Changed("machine") {
		flagsCfg.MachineID = machineID
	}
	if cmd.Flags().Changed("interval") {
		intervalDuration, parseErr := time.ParseDuration(interval)
		if parseErr != nil {
			log.Fatalf("invalid interval duration: %v", parseErr)
		}
		flagsCfg.Interval = intervalDuration
	}
	if cmd.Flags().Changed("insecure") {
		flagsCfg.Insecure = insecure
	}
	if cmd.Flags().Changed("log-level") {
		flagsCfg.LogLevel = logLevel
	}
	if cmd.Flags().Changed("cloud-provisioned") {
		flagsCfg.CloudProvisioned = cloudProvisioned
	}

	cfg := config.Merge(fileCfg, flagsCfg)
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	// Validate
	if err := config.Validate(cfg); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	shouldPersist := cmd.Flags().Changed("server") ||
		cmd.Flags().Changed("token") ||
		cmd.Flags().Changed("machine") ||
		cmd.Flags().Changed("interval") ||
		cmd.Flags().Changed("insecure") ||
		cmd.Flags().Changed("log-level") ||
		cmd.Flags().Changed("cloud-provisioned")
	if shouldPersist {
		if err := config.Save(configPath, cfg); err != nil {
			log.Fatalf("persist config: %v", err)
		}
	}

	return cfg
}

type outcomeBuffer struct {
	mu     sync.Mutex
	events []types.OutcomeEvent
}

type commandDispatcher struct {
	ctx            context.Context
	executor       *commands.Executor
	client         *client.Client
	heavyQueue     chan types.AgentCommand
	lightSemaphore chan struct{}
	activeCount    atomic.Int64
}

func newCommandDispatcher(
	ctx context.Context,
	executor *commands.Executor,
	cl *client.Client,
	lightConcurrency int,
) *commandDispatcher {
	if lightConcurrency < 1 {
		lightConcurrency = 1
	}
	dispatcher := &commandDispatcher{
		ctx:            ctx,
		executor:       executor,
		client:         cl,
		heavyQueue:     make(chan types.AgentCommand, heavyCommandQueueSize),
		lightSemaphore: make(chan struct{}, lightConcurrency),
	}
	go dispatcher.runHeavyWorker()
	return dispatcher
}

func (d *commandDispatcher) runHeavyWorker() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case command := <-d.heavyQueue:
			d.execute(command)
		}
	}
}

func (d *commandDispatcher) EnqueueBatch(commandsBatch []types.AgentCommand) {
	for _, command := range commandsBatch {
		d.enqueue(command)
	}
}

func (d *commandDispatcher) enqueue(command types.AgentCommand) {
	d.activeCount.Add(1)
	if commands.CommandLane(command.Type) == commands.CommandLaneHeavy {
		select {
		case d.heavyQueue <- command:
			return
		case <-d.ctx.Done():
			d.activeCount.Add(-1)
			return
		}
	}
	go d.runLight(command)
}

func (d *commandDispatcher) runLight(command types.AgentCommand) {
	select {
	case d.lightSemaphore <- struct{}{}:
	case <-d.ctx.Done():
		d.activeCount.Add(-1)
		return
	}
	defer func() { <-d.lightSemaphore }()
	d.execute(command)
}

func (d *commandDispatcher) execute(command types.AgentCommand) {
	defer d.activeCount.Add(-1)
	result, err := d.executor.ExecuteWithContext(d.ctx, command)
	status := "success"
	if err != nil {
		status = "failed"
		if strings.TrimSpace(result.Details) == "" {
			result.Details = err.Error()
		}
		log.Printf("command %s (%s) failed: %v", command.ID, command.Type, err)
	} else {
		log.Printf("command %s (%s) completed", command.ID, command.Type)
	}
	ackErr := ackCommandWithRetry(d.client, types.AckRequest{
		CommandID:     command.ID,
		Status:        status,
		Details:       result.Details,
		ResultPayload: result.ResultPayload,
	})
	if ackErr != nil {
		log.Printf("ack failed for %s: %v", command.ID, ackErr)
	}
}

func (d *commandDispatcher) HasActiveWork() bool {
	return d.activeCount.Load() > 0
}

func (d *commandDispatcher) WaitForIdle(ctx context.Context) bool {
	for {
		if !d.HasActiveWork() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *outcomeBuffer) Add(event types.OutcomeEvent) {
	if strings.TrimSpace(event.EventType) == "" {
		return
	}
	if strings.TrimSpace(event.Timestamp) == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	b.mu.Lock()
	b.events = append(b.events, event)
	b.mu.Unlock()
}

func (b *outcomeBuffer) Snapshot() []types.OutcomeEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.events) == 0 {
		return nil
	}
	result := make([]types.OutcomeEvent, len(b.events))
	copy(result, b.events)
	return result
}

func (b *outcomeBuffer) DropPrefix(count int) {
	if count <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if count >= len(b.events) {
		b.events = nil
		return
	}
	b.events = append([]types.OutcomeEvent{}, b.events[count:]...)
}

func runCheckIn(
	ctx context.Context,
	cfg config.Config,
	cl *client.Client,
	runtimeManager *runtimeagent.Manager,
	trainerManager *trainer.Manager,
	executor *commands.Executor,
	outcomes *outcomeBuffer,
	dispatcher *commandDispatcher,
	startedAt time.Time,
) bool {
	cachedDesired := loadCachedDesiredConfig()

	report := discovery.Collect()
	installedRuntimeNames := runtimeManager.InstalledRuntimes()
	installedRuntimeTypes := toRuntimeTypes(installedRuntimeNames)
	readyRuntimeNames := runtimeManager.ReadyRuntimes()
	runtimeStatuses := buildRuntimeStatuses(installedRuntimeNames, readyRuntimeNames)
	installedModels := toInstalledModels(runtimeManager)
	hasActiveWork := hasActiveOperations(runtimeStatuses, installedModels, trainerManager.HasActiveJobs())
	runtimeStatus := types.RuntimeNotInstalled
	runtimeType := types.RuntimeType("")
	runtimeVersion := ""
	if len(installedRuntimeNames) > 0 {
		runtimeStatus = types.RuntimeInstalling
		runtimeType = types.RuntimeType(installedRuntimeNames[0])
		runtimeVersion = runtimeManager.RuntimeVersion(installedRuntimeNames[0])
	}
	if len(readyRuntimeNames) > 0 {
		runtimeStatus = types.RuntimeReady
		runtimeType = types.RuntimeType(readyRuntimeNames[0])
		runtimeVersion = runtimeManager.RuntimeVersion(readyRuntimeNames[0])
	}

	pendingOutcomes := outcomes.Snapshot()
	payload := types.CheckinRequest{
		MachineID:              cfg.MachineID,
		Hostname:               report.Hostname,
		Addresses:              report.Addresses,
		OS:                     report.OS,
		CPUArch:                report.CPUArch,
		GPUVendor:              report.GPUVendor,
		HardwareSummary:        report.HardwareSummary,
		RAMTotalMB:             report.RAMTotalMB,
		GPUs:                   toProtocolGPUInfo(report.GPUs),
		Interconnect:           report.Interconnect,
		AgentVersion:           agentVersion,
		AgentHealth:            computeAgentHealth(installedModels),
		RuntimeStatus:          runtimeStatus,
		RuntimeStatuses:        runtimeStatuses,
		RuntimeType:            runtimeType,
		RuntimeVersion:         runtimeVersion,
		RuntimeVersions:        runtimeManager.RuntimeVersions(),
		RuntimeConfigs:         runtimeManager.RuntimeConfigs(),
		InstalledRuntimeTypes:  installedRuntimeTypes,
		InstalledTrainers:      trainerManager.InstalledTrainers(),
		InstalledModels:        installedModels,
		InstalledHarnesses:     toInstalledHarnesses(cachedDesired),
		InstalledOrchestrators: toInstalledOrchestrators(cachedDesired),
		OutcomeEvents:          pendingOutcomes,
		Uptime:                 int64(time.Since(startedAt).Seconds()),
		LoadAvg:                readLoadAvg(),
	}
	if origin := configOrigin(cfg); origin != nil {
		payload.Origin = origin
	}

	resp, err := client.Retry(ctx, 3, func() (types.CheckinResponse, error) {
		return cl.Checkin(ctx, payload)
	})
	if err != nil {
		log.Printf("checkin error: %v", err)
		enforceDesiredConfig(runtimeManager, cachedDesired)
		return hasActiveWork
	}
	if len(pendingOutcomes) > 0 {
		outcomes.DropPrefix(len(pendingOutcomes))
	}

	if err := saveCachedDesiredConfig(resp.DesiredConfig); err != nil {
		log.Printf("failed to persist desired config cache: %v", err)
	}
	desiredHarnesses := toInstalledHarnesses(resp.DesiredConfig)
	desiredOrchestrators := toInstalledOrchestrators(resp.DesiredConfig)
	if harnessReportsDiffer(payload.InstalledHarnesses, desiredHarnesses) || orchestratorReportsDiffer(payload.InstalledOrchestrators, desiredOrchestrators) {
		refreshPayload := payload
		refreshPayload.InstalledHarnesses = desiredHarnesses
		refreshPayload.InstalledOrchestrators = desiredOrchestrators
		// Outcome events were already sent in the main check-in above.
		refreshPayload.OutcomeEvents = nil
		refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer refreshCancel()
		if _, err := cl.Checkin(refreshCtx, refreshPayload); err != nil {
			log.Printf("follow-up harness/orchestrator refresh checkin failed: %v", err)
		}
	}
	enforceDesiredConfig(runtimeManager, resp.DesiredConfig)
	reconcileStaleModels(runtimeManager, resp.DesiredConfig, executor.ActiveManifestModelIDs(cfg.MachineID))
	if len(installedRuntimeNames) == 0 && resp.Recommendations != nil && len(resp.Recommendations.Stacks) > 0 {
		log.Printf("Recommended stack available. Run 'mantler recommend' for details or 'mantler setup --recommended' to install.")
	}

	// Execute commands asynchronously across heavy/light lanes.
	if dispatcher != nil && len(resp.Commands) > 0 {
		dispatcher.EnqueueBatch(resp.Commands)
	}
	return hasActiveWork || (dispatcher != nil && dispatcher.HasActiveWork())
}

func readLoadAvg() []float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	fields := strings.Fields(strings.TrimSpace(string(raw)))
	if len(fields) < 3 {
		return nil
	}
	values := make([]float64, 0, 3)
	for i := 0; i < 3; i++ {
		parsed, parseErr := strconv.ParseFloat(fields[i], 64)
		if parseErr != nil {
			return nil
		}
		values = append(values, parsed)
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func configOrigin(cfg config.Config) *types.MachineOrigin {
	if cfg.Origin == nil && !cfg.CloudProvisioned {
		return nil
	}
	if cfg.Origin == nil && cfg.CloudProvisioned {
		return &types.MachineOrigin{Kind: "cloud_compute"}
	}
	raw, err := json.Marshal(cfg.Origin)
	if err != nil {
		return &types.MachineOrigin{Kind: "cloud_compute"}
	}
	var origin types.MachineOrigin
	if err := json.Unmarshal(raw, &origin); err != nil {
		return &types.MachineOrigin{Kind: "cloud_compute"}
	}
	if strings.TrimSpace(origin.Kind) == "" {
		if cfg.CloudProvisioned {
			origin.Kind = "cloud_compute"
		} else {
			origin.Kind = "local"
		}
	}
	return &origin
}

func toProtocolGPUInfo(values []discovery.GPUInfo) []types.GPUInfo {
	if len(values) == 0 {
		return nil
	}
	result := make([]types.GPUInfo, 0, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value.Name)
		if name == "" {
			continue
		}
		result = append(result, types.GPUInfo{
			Name:              name,
			MemoryTotalMB:     value.MemoryTotalMB,
			MemoryUsedMB:      value.MemoryUsedMB,
			MemoryFreeMB:      value.MemoryFreeMB,
			Architecture:      strings.TrimSpace(value.Architecture),
			ComputeCapability: strings.TrimSpace(value.ComputeCapability),
			UnifiedMemory:     value.UnifiedMemory,
		})
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func buildRuntimeStatuses(installedRuntimeNames []string, readyRuntimeNames []string) map[types.RuntimeType]types.RuntimeStatus {
	statuses := make(map[types.RuntimeType]types.RuntimeStatus, len(installedRuntimeNames))
	for _, runtimeName := range installedRuntimeNames {
		statuses[types.RuntimeType(runtimeName)] = types.RuntimeInstalling
	}
	for _, runtimeName := range readyRuntimeNames {
		statuses[types.RuntimeType(runtimeName)] = types.RuntimeReady
	}
	if len(statuses) == 0 {
		return nil
	}
	return statuses
}

func sendInProgressAck(cl *client.Client, payload types.AckRequest) {
	if payload.CommandID == "" || (payload.Details == "" && len(payload.StreamEvents) == 0) {
		return
	}
	payload.Status = "in_progress"
	err := cl.Ack(context.Background(), payload)
	if err != nil {
		log.Printf("progress ack failed for %s: %v", payload.CommandID, err)
	}
}

func computeAgentHealth(models []types.InstalledModel) types.AgentHealth {
	for _, model := range models {
		switch model.Status {
		case types.ModelDownloading, types.ModelDownloaded, types.ModelInstalling, types.ModelBuilding, types.ModelBuilt, types.ModelStarting, types.ModelStopping:
			return types.AgentBusy
		case types.ModelFailed:
			return types.AgentDegraded
		}
	}
	return types.AgentHealthy
}

func hasActiveOperations(runtimeStatuses map[types.RuntimeType]types.RuntimeStatus, models []types.InstalledModel, hasActiveTraining bool) bool {
	if hasActiveTraining {
		return true
	}
	for _, runtimeStatus := range runtimeStatuses {
		if runtimeStatus == types.RuntimeInstalling {
			return true
		}
	}
	for _, model := range models {
		switch model.Status {
		case types.ModelDownloading, types.ModelDownloaded, types.ModelInstalling, types.ModelBuilding, types.ModelBuilt, types.ModelStarting, types.ModelStopping:
			return true
		}
	}
	return false
}

func nextCheckinInterval(idleInterval time.Duration, active bool) time.Duration {
	if !active {
		return idleInterval
	}
	const activeInterval = 15 * time.Second
	if idleInterval <= activeInterval {
		return idleInterval
	}
	return activeInterval
}
