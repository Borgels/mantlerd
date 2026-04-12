package tools

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var numericPattern = regexp.MustCompile(`\d+(\.\d+)?`)
var bandwidthWordPattern = regexp.MustCompile(`\bbandwidth\b`)
const maxReasonableBandwidthGBps = 10000.0

func parseMaxBandwidthFromText(text string) float64 {
	maxValue := 0.0
	inBandwidthBlock := false
	for _, line := range strings.Split(text, "\n") {
		normalizedLine := strings.ToLower(strings.TrimSpace(line))
		if normalizedLine == "" {
			inBandwidthBlock = false
			continue
		}
		hasBandwidthKeyword := bandwidthWordPattern.MatchString(normalizedLine) ||
			strings.Contains(normalizedLine, "gb/s") ||
			strings.Contains(normalizedLine, "gbs")
		if hasBandwidthKeyword {
			inBandwidthBlock = true
		}
		if !hasBandwidthKeyword && !inBandwidthBlock {
			continue
		}
		if strings.Contains(normalizedLine, "version") {
			continue
		}
		for _, match := range numericPattern.FindAllString(normalizedLine, -1) {
			value, err := strconv.ParseFloat(match, 64)
			if err != nil {
				continue
			}
			if value <= 0 || value > maxReasonableBandwidthGBps {
				continue
			}
			if value > maxValue {
				maxValue = value
			}
		}
	}
	return maxValue
}

func parseBandwidthTestOutputGBps(text string) float64 {
	maxMBps := 0.0
	inTable := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			inTable = false
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "bandwidth(mb/s)") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		candidate := fields[len(fields)-1]
		value, err := strconv.ParseFloat(candidate, 64)
		if err != nil {
			continue
		}
		if value > maxMBps {
			maxMBps = value
		}
	}
	if maxMBps <= 0 {
		return 0
	}
	return maxMBps / 1000
}

func parseBandwidthFromJSON(text string) float64 {
	var payload any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return 0
	}
	return extractBandwidthValue(payload, false)
}

func extractBandwidthValue(value any, inBandwidthContext bool) float64 {
	switch typed := value.(type) {
	case map[string]any:
		best := 0.0
		for key, candidate := range typed {
			next := extractBandwidthValue(candidate, inBandwidthContext || isBandwidthKey(key))
			if next > best {
				best = next
			}
		}
		return best
	case []any:
		best := 0.0
		for _, candidate := range typed {
			next := extractBandwidthValue(candidate, inBandwidthContext)
			if next > best {
				best = next
			}
		}
		return best
	case float64:
		if !inBandwidthContext {
			return 0
		}
		if typed <= 0 || typed > maxReasonableBandwidthGBps {
			return 0
		}
		return typed
	case string:
		if !inBandwidthContext {
			return 0
		}
		return parseMaxBandwidthFromText(typed)
	default:
		return 0
	}
}

func isBandwidthKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(normalized, "bandwidth") ||
		strings.Contains(normalized, "gbps") ||
		strings.Contains(normalized, "gb_per_s") ||
		strings.Contains(normalized, "throughput") ||
		normalized == "bw"
}
