package load

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/scenario"
)

// agentCounter counts open calls as the agent sees them, independently of
// the runner's own bookkeeping.
type agentCounter struct {
	mu                  sync.Mutex
	open, peak, started int
	stopped             int
}

func (a *agentCounter) event(e fakeagent.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch e.Kind {
	case fakeagent.EventStarted:
		a.open++
		a.started++
		a.peak = max(a.peak, a.open)
	case fakeagent.EventStopped:
		a.open--
		a.stopped++
	}
}

func (a *agentCounter) counts() (peak, started, stopped int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.peak, a.started, a.stopped
}

func agent(t *testing.T, cfg fakeagent.Config) (string, *agentCounter) {
	t.Helper()
	counter := &agentCounter{}
	cfg.OnEvent = counter.event
	srv := httptest.NewServer(fakeagent.New(cfg))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), counter
}

// loadScenario writes a short clip and the given scenario, and loads it.
func loadScenario(t *testing.T, yaml string) *scenario.Scenario {
	t.Helper()
	dir := t.TempDir()
	pcm := make([]int16, audio.TelephonySampleRate/5) // 200 ms tone
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
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// waitStops waits for the agent to have seen n hang-ups.
func waitStops(t *testing.T, a *agentCounter, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, stopped := a.counts(); stopped >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, stopped := a.counts()
	t.Errorf("agent saw %d hang-ups, want %d: calls were left open", stopped, n)
}

func TestRampSchedule(t *testing.T) {
	p := Plan{Concurrency: 5, RampUp: 800 * time.Millisecond}
	for _, tc := range []struct {
		at   time.Duration
		want int
	}{{0, 1}, {199 * time.Millisecond, 1}, {200 * time.Millisecond, 2}, {799 * time.Millisecond, 4}, {800 * time.Millisecond, 5}, {time.Hour, 5}} {
		if got := p.allowed(tc.at); got != tc.want {
			t.Errorf("allowed(%v) = %d, want %d", tc.at, got, tc.want)
		}
	}
	if got := p.nextStep(1); got != 200*time.Millisecond {
		t.Errorf("nextStep(1) = %v", got)
	}
	if got := p.nextStep(4); got != 800*time.Millisecond {
		t.Errorf("nextStep(4) = %v", got)
	}
	if got := (&Plan{Concurrency: 5}).allowed(0); got != 5 {
		t.Errorf("no ramp: allowed(0) = %d, want 5", got)
	}
}

func TestValidate(t *testing.T) {
	sc := &scenario.Scenario{}
	ok := Plan{URL: "ws://x", Scenario: sc, Calls: 1, Concurrency: 1}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Plan){
		"no url":           func(p *Plan) { p.URL = "" },
		"no scenario":      func(p *Plan) { p.Scenario = nil },
		"zero calls":       func(p *Plan) { p.Calls = 0 },
		"zero concurrency": func(p *Plan) { p.Concurrency = 0 },
		"negative ramp":    func(p *Plan) { p.RampUp = -time.Second },
		"negative timeout": func(p *Plan) { p.CallTimeout = -time.Second },
	} {
		p := ok
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := Run(context.Background(), p); err == nil {
			t.Errorf("%s: Run accepted", name)
		}
	}
}

// Every call runs the scenario end to end and gets metrics.
func TestRunsAllCalls(t *testing.T) {
	url, counter := agent(t, fakeagent.Config{EndpointSilence: 100 * time.Millisecond, Burst: true})
	sc := loadScenario(t, "reply_gap: 300ms\nsteps:\n  - say: hello.wav\n")
	var progress atomic.Int32
	rep, err := Run(context.Background(), Plan{
		URL: url, Scenario: sc, Calls: 6, Concurrency: 3,
		OnCall: func(Call) { progress.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Calls) != 6 || rep.Failed() != 0 || rep.Cancelled {
		t.Fatalf("calls=%d failed=%d cancelled=%v", len(rep.Calls), rep.Failed(), rep.Cancelled)
	}
	for i, c := range rep.Calls {
		if c.ID != i+1 {
			t.Errorf("calls not in ID order: position %d has ID %d", i, c.ID)
		}
		if len(c.Metrics.Turns) != 1 || !c.Metrics.Turns[0].Replied {
			t.Errorf("call %d: metrics %+v", c.ID, c.Metrics)
		}
		if c.Result.Timeline == nil || c.Result.Agent.Frames != nil {
			t.Errorf("call %d: want timeline kept and agent audio dropped", c.ID)
		}
		if c.Pacing.FramesSent == 0 {
			t.Errorf("call %d: no pacing stats", c.ID)
		}
	}
	if progress.Load() != 6 {
		t.Errorf("OnCall called %d times", progress.Load())
	}
	waitStops(t, counter, 6)
}

func TestConcurrencyLimit(t *testing.T) {
	url, counter := agent(t, fakeagent.Config{})
	sc := loadScenario(t, "steps:\n  - pause: 300ms\n")
	rep, err := Run(context.Background(), Plan{URL: url, Scenario: sc, Calls: 12, Concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	waitStops(t, counter, 12)
	peak, started, _ := counter.counts()
	if peak != 4 || rep.MaxActive != 4 || started != 12 {
		t.Errorf("agent peak %d, runner peak %d, started %d; want 4, 4, 12", peak, rep.MaxActive, started)
	}
	// Three waves of four 300 ms calls.
	if d := rep.Ended.Sub(rep.Started); d < 850*time.Millisecond || d > 2*time.Second {
		t.Errorf("run took %v, want about 900ms", d)
	}
}

func TestRampUpStartTimes(t *testing.T) {
	url, _ := agent(t, fakeagent.Config{})
	sc := loadScenario(t, "steps:\n  - pause: 1500ms\n")
	rep, err := Run(context.Background(), Plan{
		URL: url, Scenario: sc, Calls: 5, Concurrency: 5, RampUp: 800 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range rep.Calls {
		want := time.Duration(i) * 200 * time.Millisecond
		got := c.Started.Sub(rep.Started)
		if got < want-20*time.Millisecond || got > want+80*time.Millisecond {
			t.Errorf("call %d started at %v, want %v", c.ID, got, want)
		}
		if c.ActiveAtStart != i+1 {
			t.Errorf("call %d started with %d active, want %d", c.ID, c.ActiveAtStart, i+1)
		}
	}
}

func TestCallTimeout(t *testing.T) {
	url, counter := agent(t, fakeagent.Config{})
	sc := loadScenario(t, "steps:\n  - pause: 5s\n")
	rep, err := Run(context.Background(), Plan{
		URL: url, Scenario: sc, Calls: 3, Concurrency: 3, CallTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Calls {
		if c.Err == nil || !strings.Contains(c.Err.Error(), "timed out after 300ms") {
			t.Errorf("call %d: err = %v", c.ID, c.Err)
		}
		if d := c.Ended.Sub(c.Started); d > time.Second {
			t.Errorf("call %d ran %v despite a 300ms timeout", c.ID, d)
		}
	}
	if rep.Cancelled {
		t.Error("timeouts are not cancellation")
	}
	waitStops(t, counter, 3)
}

func TestCancellation(t *testing.T) {
	url, counter := agent(t, fakeagent.Config{})
	sc := loadScenario(t, "steps:\n  - pause: 3s\n")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	rep, err := Run(ctx, Plan{URL: url, Scenario: sc, Calls: 50, Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Run took %v to stop after cancellation", d)
	}
	if !rep.Cancelled || len(rep.Calls) != 3 {
		t.Fatalf("cancelled=%v calls=%d, want true and the 3 that started", rep.Cancelled, len(rep.Calls))
	}
	for _, c := range rep.Calls {
		if !errors.Is(c.Err, context.Canceled) {
			t.Errorf("call %d: err = %v, want context.Canceled", c.ID, c.Err)
		}
	}
	waitStops(t, counter, 3)
}

func TestDialFailures(t *testing.T) {
	sc := loadScenario(t, "steps:\n  - pause: 100ms\n")
	rep, err := Run(context.Background(), Plan{
		URL: "ws://127.0.0.1:1/nothing", Scenario: sc, Calls: 3, Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() != 3 {
		t.Fatalf("failed = %d, want 3", rep.Failed())
	}
	for _, c := range rep.Calls {
		if c.Result != nil || !strings.Contains(c.Err.Error(), "dial") {
			t.Errorf("call %d: result=%v err=%v", c.ID, c.Result != nil, c.Err)
		}
	}
}
