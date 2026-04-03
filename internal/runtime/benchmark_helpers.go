package runtime

import (
	"math"
	"sort"
	"strings"
)

type BenchmarkResult struct {
	TTFTMs                      float64
	OutputTokensPerSec          float64
	TotalLatencyMs              float64
	PromptTokensPerSec          float64
	P95TTFTMsAtSmallConcurrency float64
}

type BenchmarkProgress struct {
	RunsCompleted    int
	RunsTotal        int
	SuccessfulRuns   int
	FailedRuns       int
	LastRunLatencyMs float64
	Benchmark        *BenchmarkResult
}

func makeBenchmarkPrompt(tokenCount int) string {
	if tokenCount < 32 {
		tokenCount = 32
	}
	var builder strings.Builder
	builder.WriteString("Summarize this synthetic benchmark context in concise bullet points.\n")
	for i := 0; i < tokenCount; i++ {
		builder.WriteString("token ")
	}
	return builder.String()
}

func roundTo(value float64, decimals int) float64 {
	pow := math.Pow10(decimals)
	return math.Round(value*pow) / pow
}

func summarizeBenchmarkResults(results []BenchmarkResult) BenchmarkResult {
	if len(results) == 0 {
		return BenchmarkResult{}
	}
	ttftValues := make([]float64, 0, len(results))
	var sumTTFT, sumOutputTPS, sumPromptTPS, sumLatency float64
	for _, item := range results {
		ttftValues = append(ttftValues, item.TTFTMs)
		sumTTFT += item.TTFTMs
		sumOutputTPS += item.OutputTokensPerSec
		sumPromptTPS += item.PromptTokensPerSec
		sumLatency += item.TotalLatencyMs
	}
	sort.Float64s(ttftValues)
	p95Index := int(math.Ceil(float64(len(ttftValues))*0.95)) - 1
	if p95Index < 0 {
		p95Index = 0
	}
	if p95Index >= len(ttftValues) {
		p95Index = len(ttftValues) - 1
	}
	count := float64(len(results))
	return BenchmarkResult{
		TTFTMs:                      roundTo(sumTTFT/count, 2),
		OutputTokensPerSec:          roundTo(sumOutputTPS/count, 2),
		TotalLatencyMs:              roundTo(sumLatency/count, 2),
		PromptTokensPerSec:          roundTo(sumPromptTPS/count, 2),
		P95TTFTMsAtSmallConcurrency: roundTo(ttftValues[p95Index], 2),
	}
}
