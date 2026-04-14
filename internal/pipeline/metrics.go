package pipeline

import "encoding/json"

func estimateTokensFromMap(input map[string]any) int {
	raw, err := json.Marshal(input)
	if err != nil {
		return 0
	}
	// Coarse estimator used for sidecar metering when runtime tokens are unavailable.
	return len(raw) / 4
}
