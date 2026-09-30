package httpapi

import (
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/pipeline"
)

func TestRunResponseReturnsHistoricalPipelineMode(t *testing.T) {
	result := runResponse(pipeline.RunDetail{Mode: pipeline.ModeBuildOnly})
	if string(result.Mode) != pipeline.ModeBuildOnly {
		t.Fatalf("mode = %q, want %q", result.Mode, pipeline.ModeBuildOnly)
	}
}
