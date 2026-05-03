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
