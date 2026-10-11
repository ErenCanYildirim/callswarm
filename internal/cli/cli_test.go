package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/report"
)

// syncBuffer is a bytes.Buffer safe to read while a command writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func run(t *testing.T, ctx context.Context, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut syncBuffer
	code = Run(ctx, args, "v-test", &out, &errOut)
	return code, out.String(), errOut.String()
}

// writeScenario writes a one-turn scenario with a short clip.
func writeScenario(t *testing.T, steps string) string {
	t.Helper()
	dir := t.TempDir()
	pcm := make([]int16, audio.TelephonySampleRate*3/10)
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
	path := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(path, []byte("name: cli-test\nreply_gap: 400ms\nsteps:\n"+steps), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func agentURL(t *testing.T, cfg fakeagent.Config) string {
	t.Helper()
	srv := httptest.NewServer(fakeagent.New(cfg))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestUsageAndVersion(t *testing.T) {
	ctx := context.Background()
	if code, _, stderr := run(t, ctx); code != ExitUsage || !strings.Contains(stderr, "Usage:") {
		t.Errorf("no args: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run(t, ctx, "launch"); code != ExitUsage || !strings.Contains(stderr, `unknown command "launch"`) {
		t.Errorf("unknown command: code %d, stderr %q", code, stderr)
	}
	if code, stdout, _ := run(t, ctx, "version"); code != ExitOK || strings.TrimSpace(stdout) != "v-test" {
		t.Errorf("version: code %d, stdout %q", code, stdout)
	}
	if code, stdout, _ := run(t, ctx, "help"); code != ExitOK || !strings.Contains(stdout, "fake-agent") {
		t.Errorf("help: code %d", code)
	}
	if code, _, stderr := run(t, ctx, "run", "-h"); code != ExitOK || !strings.Contains(stderr, "-concurrency") {
		t.Errorf("run -h: code %d, stderr %q", code, stderr)
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	sc := writeScenario(t, "  - say: hello.wav\n")
	ctx := context.Background()
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no scenario":   {[]string{"run", "--url", "ws://x"}, "missing scenario"},
		"two scenarios": {[]string{"run", sc, sc, "--url", "ws://x"}, "expected one scenario"},
		"no url":        {[]string{"run", sc}, "--url is required"},
		"http url":      {[]string{"run", sc, "--url", "http://localhost:8080/x"}, "did you mean ws://localhost:8080/x"},
		"other scheme":  {[]string{"run", sc, "--url", "ftp://x"}, "ws:// or wss://"},
		"zero calls":    {[]string{"run", sc, "--url", "ws://x", "--calls", "0"}, "--calls"},
		"bad format":    {[]string{"run", sc, "--url", "ws://x", "--format", "xml"}, "--format"},
		"bad header":    {[]string{"run", sc, "--url", "ws://x", "--header", "NoColon"}, "Name: value"},
		"bad param":     {[]string{"run", sc, "--url", "ws://x", "--param", "novalue"}, "key=value"},
		"unknown flag":  {[]string{"run", sc, "--url", "ws://x", "--speed", "2"}, "speed"},
		"negative ramp": {[]string{"run", sc, "--url", "ws://x", "--ramp-up", "-1s"}, "--ramp-up"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := run(t, ctx, tc.args...)
			if code != ExitUsage || !strings.Contains(stderr, tc.want) {
				t.Errorf("code %d, stderr %q, want exit 2 mentioning %q", code, stderr, tc.want)
			}
		})
	}
	// A broken scenario file is a failure, not a usage error.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(bad, []byte("steps: []\n"), 0o644)
	if code, _, stderr := run(t, ctx, "run", bad, "--url", "ws://x"); code != ExitFailure || !strings.Contains(stderr, "no steps") {
		t.Errorf("bad scenario: code %d, stderr %q", code, stderr)
	}
}

func TestRunTextAndJSONFile(t *testing.T) {
	url := agentURL(t, fakeagent.Config{EndpointSilence: 200 * time.Millisecond, Burst: true})
	sc := writeScenario(t, "  - say: hello.wav\n")
	out := filepath.Join(t.TempDir(), "report.json")

	// Flags after the positional argument work too.
	code, stdout, stderr := run(t, context.Background(), "run", sc,
		"--url", url+"?token=secret", "--calls", "4", "--concurrency", "2", "--out", out, "--quiet")
	if code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("--quiet still wrote to stderr: %q", stderr)
	}
	for _, want := range []string{"callswarm v-test - cli-test", "4 calls, peak 2 concurrent", "health: HEALTHY", "4 replies to 4 turns"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "secret") {
		t.Error("URL credentials printed")
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Manifest.Calls != 4 || doc.Manifest.Version != "v-test" || len(doc.Calls) != 4 || doc.Calls[0].Timeline == nil {
		t.Errorf("report file: manifest %+v, %d calls", doc.Manifest, len(doc.Calls))
	}
}

func TestRunJSONToStdout(t *testing.T) {
	url := agentURL(t, fakeagent.Config{EndpointSilence: 200 * time.Millisecond, Burst: true})
	sc := writeScenario(t, "  - say: hello.wav\n")
	code, stdout, stderr := run(t, context.Background(), "run", "--url", url, "--format", "json", "--timelines=false", sc)
	if code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	var doc report.Document
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not a JSON report: %v\n%s", err, stdout)
	}
	if doc.Summary.Replied != 1 || doc.Calls[0].Timeline != nil {
		t.Errorf("summary %+v, timeline included %v", doc.Summary, doc.Calls[0].Timeline != nil)
	}
	if !strings.Contains(stderr, "running \"cli-test\" (1 steps): 1 calls") {
		t.Errorf("progress header missing from stderr: %q", stderr)
	}
}

func TestRunCancelled(t *testing.T) {
	url := agentURL(t, fakeagent.Config{})
	sc := writeScenario(t, "  - pause: 5s\n")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	code, stdout, stderr := run(t, ctx, "run", sc, "--url", url, "--calls", "10", "--concurrency", "2")
	if code != ExitCancelled {
		t.Fatalf("exit %d, want %d\nstderr: %s", code, ExitCancelled, stderr)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v to stop", time.Since(start))
	}
	if !strings.Contains(stdout, "the run was cancelled") || !strings.Contains(stderr, "run cancelled") {
		t.Errorf("partial report not explained:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

// The fake-agent command serves an agent the run command can measure, and the
// measurement matches the agent's configured timing.
func TestFakeAgentCommandEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	agentOut := &syncBuffer{}
	agentDone := make(chan int)
	go func() {
		agentDone <- Run(ctx, []string{"fake-agent", "--addr", "127.0.0.1:0", "--delay", "100ms", "--endpoint", "200ms"},
			"", agentOut, agentOut)
	}()

	// The command prints its address; wait for it.
	addr := regexp.MustCompile(`ws://\S+`)
	var url string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && url == ""; time.Sleep(10 * time.Millisecond) {
		url = addr.FindString(agentOut.String())
	}
	if url == "" {
		t.Fatalf("fake agent never reported its address: %q", agentOut.String())
	}
	if !strings.Contains(agentOut.String(), "first audio 300ms") {
		t.Errorf("fake agent banner: %q", agentOut.String())
	}

	sc := writeScenario(t, "  - say: hello.wav\n  - say: hello.wav\n")
	code, stdout, stderr := run(t, context.Background(), "run", sc, "--url", url, "--calls", "3", "--concurrency", "3", "--format", "json", "--quiet")
	if code != ExitOK {
		t.Fatalf("run exit %d: %s", code, stderr)
	}
	var doc report.Document
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	p50 := time.Duration(doc.Summary.FirstAudio.P50MS * float64(time.Millisecond))
	if doc.Summary.Replied != 6 || p50 < 280*time.Millisecond || p50 > 380*time.Millisecond {
		t.Errorf("replied %d, p50 %v; want 6 replies at about 300ms", doc.Summary.Replied, p50)
	}

	cancel()
	select {
	case code := <-agentDone:
		if code != ExitOK {
			t.Errorf("fake agent exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake agent did not shut down")
	}
}
