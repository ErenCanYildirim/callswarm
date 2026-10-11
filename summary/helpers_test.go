package summary

import (
	"context"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/load"
	"github.com/ErenCanYildirim/callswarm/scenario"
)

// agentCfg answers 500 ms after the caller stops speaking.
var agentCfg = fakeagent.Config{EndpointSilence: 300 * time.Millisecond, ResponseDelay: 200 * time.Millisecond}

// runLoad makes n concurrent two-turn calls against a fake agent.
func runLoad(t *testing.T, n int) *load.Report {
	t.Helper()
	srv := httptest.NewServer(fakeagent.New(agentCfg))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	pcm := make([]int16, audio.TelephonySampleRate*3/10) // 300 ms tone
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/audio.TelephonySampleRate))
	}
	f, err := os.Create(filepath.Join(dir, "hello.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if err := audio.EncodeWAV(f, audio.PCM{SampleRate: audio.TelephonySampleRate, Samples: pcm}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s.yaml")
	yaml := "reply_gap: 400ms\nsteps:\n  - say: hello.wav\n  - say: hello.wav\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := load.Run(context.Background(), load.Plan{
		URL:      "ws" + strings.TrimPrefix(srv.URL, "http"),
		Scenario: sc, Calls: n, Concurrency: n, RampUp: 2 * time.Second, CallTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}
