package commands

import (
	"fmt"

	"github.com/Borgels/clawcontrol-agent/internal/runtime"
	"github.com/Borgels/clawcontrol-agent/internal/types"
)

type Executor struct {
	runtimeManager *runtime.Manager
}

func NewExecutor(runtimeManager *runtime.Manager) *Executor {
	return &Executor{
		runtimeManager: runtimeManager,
	}
}

func (e *Executor) Execute(command types.AgentCommand) error {
	switch command.Type {
	case "install_runtime":
		rawRuntime, ok := command.Params["runtime"]
		if !ok {
			return fmt.Errorf("missing runtime param")
		}
		runtimeName, ok := rawRuntime.(string)
		if !ok || runtimeName == "" {
			return fmt.Errorf("invalid runtime param")
		}
		return e.runtimeManager.InstallRuntime(runtimeName)
	case "pull_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return err
		}
		flags := modelFeatureFlagsParam(command.Params)
		return e.runtimeManager.EnsureModelWithFlags(modelID, flags)
	case "remove_model":
		modelID, err := stringParam(command.Params, "modelId")
		if err != nil {
			return err
		}
		return e.runtimeManager.RemoveModel(modelID)
	case "health_check":
		return nil
	case "restart_runtime":
		return e.runtimeManager.RestartRuntime()
	case "update_agent":
		// Reserved for future signed self-update flow.
		return nil
	default:
		return fmt.Errorf("unsupported command type: %s", command.Type)
	}
}

func stringParam(params map[string]interface{}, key string) (string, error) {
	raw, ok := params[key]
	if !ok {
		return "", fmt.Errorf("missing %s param", key)
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return "", fmt.Errorf("invalid %s param", key)
	}
	return value, nil
}

func modelFeatureFlagsParam(params map[string]interface{}) *types.ModelFeatureFlags {
	raw, ok := params["featureFlags"]
	if !ok {
		return nil
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	flags := &types.ModelFeatureFlags{
		Streaming: true,
		Thinking:  false,
	}
	if streaming, ok := obj["streaming"].(bool); ok {
		flags.Streaming = streaming
	}
	if thinking, ok := obj["thinking"].(bool); ok {
		flags.Thinking = thinking
	}
	return flags
}
