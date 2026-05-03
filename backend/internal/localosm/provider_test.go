package localosm

import "testing"

func TestClassifySurface(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
		want int
	}{
		{
			name: "asphalt is paved",
			tags: map[string]string{"surface": "asphalt"},
			want: SurfacePaved,
		},
		{
			name: "gravel is unpaved",
			tags: map[string]string{"surface": "gravel"},
			want: SurfaceUnpaved,
		},
		{
			name: "track grade1 is paved",
			tags: map[string]string{"tracktype": "grade1"},
			want: SurfacePaved,
		},
		{
			name: "missing surface is unknown",
			tags: map[string]string{},
			want: SurfaceUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifySurface(tt.tags); got != tt.want {
				t.Fatalf("classifySurface() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLocalScorePenalizesRoutesMoreThanHalfKilometerLong(t *testing.T) {
	targetM := 10000.0

	withinLimit := localCandidate{
		DistanceM:    10500,
		PavedPercent: 70,
	}
	overLimit := localCandidate{
		DistanceM:    10600,
		PavedPercent: 70,
	}

	if localScore(withinLimit, targetM, 70) >= localScore(overLimit, targetM, 70) {
		t.Fatal("expected local scoring to penalize routes more than 0.5 km longer than requested")
	}
}

func TestHasRepeatedEdgesDetectsOutAndBack(t *testing.T) {
	if !hasRepeatedEdges([]int{1, 2, 1}) {
		t.Fatal("expected an out-and-back path to count as a repeated road segment")
	}
}

func TestHasRepeatedEdgesAllowsSimpleLoop(t *testing.T) {
	if hasRepeatedEdges([]int{1, 2, 3, 1}) {
		t.Fatal("expected a simple loop without repeated segments to be allowed")
	}
}
