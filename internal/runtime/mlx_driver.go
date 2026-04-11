package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Borgels/mantlerd/internal/types"
)

type mlxDriver struct{}

type mlxModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func newMLXDriver() Driver {
	return &mlxDriver{}
}

func (d *mlxDriver) Name() string { return "mlxserver" }

func (d *mlxDriver) Install() error {
	return fmt.Errorf("automatic MLX-server install is not supported; install and start MLX-server manually")
}

func (d *mlxDriver) Uninstall() error {
	return fmt.Errorf("automatic MLX-server uninstall is not supported; remove MLX-server manually")
}

func (d *mlxDriver) IsInstalled() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	if runCommand("sh", "-c", "command -v mlx-server") == nil {
		return true
	}
	if runCommand("python3", "-c", "import mlx_lm") == nil {
		return true
	}
	return false
}

func (d *mlxDriver) IsReady() bool {
	req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/v1/models", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (d *mlxDriver) Version() string {
	if output, err := exec.Command("mlx-server", "--version").CombinedOutput(); err == nil {
		return strings.TrimSpace(string(output))
	}
	if output, err := exec.Command("python3", "-c", "import importlib.metadata as m; print(m.version('mlx-lm'))").CombinedOutput(); err == nil {
		return strings.TrimSpace(string(output))
	}
	return ""
}

func (d *mlxDriver) EnsureModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.StartModelWithFlags(modelID, flags)
}

func (d *mlxDriver) PrepareModelWithFlags(modelID string, _ *types.ModelFeatureFlags) error {
	if strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("model ID is required")
	}
	if !d.IsReady() {
		return fmt.Errorf("MLX-server is not reachable at %s", d.baseURL())
	}
	return nil
}

func (d *mlxDriver) StartModelWithFlags(modelID string, flags *types.ModelFeatureFlags) error {
	return d.PrepareModelWithFlags(modelID, flags)
}

func (d *mlxDriver) StopModel(modelID string) error {
	_ = modelID
	return nil
}

func (d *mlxDriver) ListModels() []string {
	req, err := http.NewRequest(http.MethodGet, d.baseURL()+"/v1/models", nil)
	if err != nil {
		return nil
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var parsed mlxModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	models := make([]string, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" {
			models = append(models, id)
		}
	}
	return models
}

func (d *mlxDriver) HasModel(modelID string) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	for _, model := range d.ListModels() {
		if strings.EqualFold(model, modelID) {
			return true
		}
	}
	return false
}

func (d *mlxDriver) RemoveModel(modelID string) error {
	_ = modelID
	return nil
}

func (d *mlxDriver) BenchmarkModel(
	modelID string,
	samplePromptTokens int,
	sampleOutputTokens int,
	concurrency int,
	runs int,
	onProgress func(BenchmarkProgress),
) (BenchmarkResult, error) {
	_ = modelID
	_ = samplePromptTokens
	_ = sampleOutputTokens
	_ = concurrency
	_ = runs
	_ = onProgress
	return BenchmarkResult{}, fmt.Errorf("MLX-server benchmarking is not yet implemented")
}

func (d *mlxDriver) RestartRuntime() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("MLX-server restart is only supported on macOS")
	}
	return fmt.Errorf("restart MLX-server manually (or via your launchd service) and retry")
}

func (d *mlxDriver) baseURL() string {
	base := strings.TrimSpace(os.Getenv("MLX_SERVER_URL"))
	if base == "" {
		base = "http://127.0.0.1:8000"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	if strings.Contains(base, "://0.0.0.0") {
		base = strings.Replace(base, "://0.0.0.0", "://127.0.0.1", 1)
	}
	return strings.TrimRight(base, "/")
}
