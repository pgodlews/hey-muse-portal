package heymuse

import (
	"math"
	"testing"
)

// A 16 kHz tone upsampled in uneven chunks is the same tone at 24 kHz, one and a half times as long.
func TestUpsamplerKeepsTheTone(t *testing.T) {
	const hz = 440.0
	in := make([]int16, 16000)
	for i := range in {
		in[i] = int16(10000 * math.Sin(2*math.Pi*hz*float64(i)/16000))
	}
	var u upsampler
	var out []byte
	for start, step := 0, 1; start < len(in); start, step = start+step, step%317+1 {
		end := min(len(in), start+step)
		out = append(out, u.feed(in[start:end])...)
	}
	n := len(out) / 2
	if n < 23990 || n > 24000 {
		t.Fatalf("got %d samples, want about 24000", n)
	}
	worst := 0.0
	for j := 0; j < n; j++ {
		got := float64(int16(uint16(out[2*j]) | uint16(out[2*j+1])<<8))
		want := 10000 * math.Sin(2*math.Pi*hz*float64(j)/24000)
		worst = math.Max(worst, math.Abs(got-want))
	}
	if worst > 300 {
		t.Fatalf("worst error %.0f, want under 300", worst)
	}
}
