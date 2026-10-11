package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ErenCanYildirim/callswarm/check"
	"github.com/ErenCanYildirim/callswarm/load"
	"github.com/ErenCanYildirim/callswarm/metrics"
	"github.com/ErenCanYildirim/callswarm/report"
	"github.com/ErenCanYildirim/callswarm/scenario"
	"github.com/ErenCanYildirim/callswarm/summary"
)

// progressInterval is how often progress is printed during a run.
const progressInterval = 5 * time.Second

// headerFlag collects repeated --header "Name: value" flags.
type headerFlag struct{ h http.Header }

func (f *headerFlag) String() string { return "" }

func (f *headerFlag) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return fmt.Errorf("want \"Name: value\", got %q", v)
	}
	if f.h == nil {
		f.h = http.Header{}
	}
	f.h.Add(name, strings.TrimSpace(value))
	return nil
}

// limitFlag collects repeated stat=duration limits.
type limitFlag struct{ limits []check.Limit }

func (f *limitFlag) String() string { return "" }

func (f *limitFlag) Set(v string) error {
	l, err := check.ParseLimit(v)
	if err != nil {
		return err
	}
	f.limits = append(f.limits, l)
	return nil
}

// paramFlag collects repeated --param key=value flags.
type paramFlag struct{ m map[string]string }

func (f *paramFlag) String() string { return "" }

func (f *paramFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[k] = val
	return nil
}

type runOptions struct {
	url         string
	calls       int
	concurrency int
	rampUp      time.Duration
	callTimeout time.Duration
	stopWithin  time.Duration
	headers     headerFlag
	params      paramFlag
	format      string
	out         string
	timelines   bool
	quiet       bool

	maxFailed      int
	failOn         string
	maxFirstAudio  limitFlag
	maxStopLatency limitFlag
	minReplyRate   float64
	minStopRate    float64
}

func runCommand(ctx context.Context, args []string, version string, stdout, stderr io.Writer) int {
	var o runOptions
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.url, "url", "", "agent WebSocket URL, ws:// or wss:// (required)")
	fs.IntVar(&o.calls, "calls", 1, "total number of calls")
	fs.IntVar(&o.concurrency, "concurrency", 1, "most calls active at once")
	fs.DurationVar(&o.rampUp, "ramp-up", 0, "time to grow from 1 to full concurrency, such as 30s")
	fs.DurationVar(&o.callTimeout, "call-timeout", load.DefaultCallTimeout, "longest a single call may run")
	fs.DurationVar(&o.stopWithin, "stop-within", metrics.DefaultStopWithin, "how quickly the agent must stop talking when interrupted")
	fs.Var(&o.headers, "header", "HTTP header for the WebSocket request, \"Name: value\" (repeatable)")
	fs.Var(&o.params, "param", "custom parameter sent in the start message, key=value (repeatable)")
	fs.StringVar(&o.format, "format", "text", "output on stdout: text or json")
	fs.StringVar(&o.out, "out", "", "also write the JSON report to this file")
	fs.BoolVar(&o.timelines, "timelines", true, "include each call's event timeline in JSON output")
	fs.BoolVar(&o.quiet, "quiet", false, "don't print progress to stderr")
	fs.IntVar(&o.maxFailed, "max-failed", 0, "check: most calls allowed to fail, -1 for any")
	fs.StringVar(&o.failOn, "fail-on", "unreliable", "check: fail when health is this bad: degraded, unreliable or never")
	fs.Var(&o.maxFirstAudio, "max-first-audio", "check: limit on time to first audio, such as p95=800ms (repeatable; mean, p50, p90, p95, p99, max)")
	fs.Var(&o.maxStopLatency, "max-stop-latency", "check: limit on time the agent talks over an interruption, such as p90=400ms (repeatable)")
	fs.Float64Var(&o.minReplyRate, "min-reply-rate", 0, "check: share of turns the agent must answer, 0 to 1 (0 disables)")
	fs.Float64Var(&o.minStopRate, "min-stop-rate", 0, "check: share of interruptions the agent must stop for within --stop-within, 0 to 1 (0 disables)")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage: callswarm run <scenario.yaml> --url <ws-url> [flags]\n\n"+
			"Exits 0 when every check passes, 3 when a check fails, 1 when the run\n"+
			"could not be carried out, 2 on a usage error and 130 when cancelled.\n"+
			"By default a run fails if any call fails or its health is unreliable.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	err := doRun(ctx, fs, args, &o, version, stdout, stderr)
	var cf checksFailed
	switch {
	case errors.Is(err, errCancelled):
		return ExitCancelled
	case errors.As(err, &cf):
		_, _ = fmt.Fprintf(stderr, "callswarm run: %s\n", cf)
		return ExitCheckFailed
	}
	return exitCode(err, "run", stderr)
}

// errCancelled marks a run stopped by the user after its report was written.
var errCancelled = errors.New("cancelled")

// checksFailed marks a completed run that did not meet its criteria.
type checksFailed struct{ failed, total int }

func (e checksFailed) Error() string {
	return fmt.Sprintf("%d of %d checks failed", e.failed, e.total)
}

func doRun(ctx context.Context, fs *flag.FlagSet, args []string, o *runOptions, version string, stdout, stderr io.Writer) error {
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageErrorf("%v", err)
	}
	if err := o.validate(positional); err != nil {
		return err
	}
	criteria := o.criteria()

	sc, err := scenario.Load(positional[0])
	if err != nil {
		return err
	}
	plan := load.Plan{
		URL:         o.url,
		Scenario:    sc,
		Calls:       o.calls,
		Concurrency: o.concurrency,
		RampUp:      o.rampUp,
		CallTimeout: o.callTimeout,
		Header:      o.headers.h,
		Parameters:  o.params.m,
		Metrics:     metrics.Options{StopWithin: o.stopWithin},
	}

	var done, failed atomic.Int64
	plan.OnCall = func(c load.Call) {
		done.Add(1)
		if c.Err != nil {
			failed.Add(1)
		}
	}
	if !o.quiet {
		_, _ = fmt.Fprintf(stderr, "running %s: %d calls, up to %d at once, against %s\n",
			describe(sc), o.calls, o.concurrency, report.RedactURL(o.url))
		stop := progress(stderr, o.calls, &done, &failed)
		defer stop()
	}

	rep, err := load.Run(ctx, plan)
	if err != nil {
		return err
	}
	th := summary.Thresholds{}
	sum := summary.Summarize(rep, th)
	man := report.NewManifest(resolveVersion(version), plan, th)
	results := check.Evaluate(sum, criteria)
	if results == nil {
		results = []check.Result{}
	}
	doc := report.Build(rep, sum, man, report.Options{Timelines: o.timelines, Checks: results})

	if o.out != "" {
		if err := writeFile(o.out, doc); err != nil {
			return fmt.Errorf("writing report: %w", err)
		}
	}
	switch o.format {
	case "json":
		err = report.WriteJSON(stdout, doc)
	default:
		err = report.WriteText(stdout, sum, man)
		if err == nil {
			err = report.WriteChecks(stdout, results)
		}
	}
	if err != nil {
		return err
	}
	if o.out != "" && !o.quiet {
		_, _ = fmt.Fprintf(stderr, "report written to %s\n", o.out)
	}
	if rep.Cancelled {
		_, _ = fmt.Fprintln(stderr, "run cancelled: the report covers the calls that started")
		return errCancelled
	}
	if n := check.Failed(results); n > 0 {
		return checksFailed{n, len(results)}
	}
	return nil
}

// criteria assembles the checks from validated flags.
func (o *runOptions) criteria() check.Criteria {
	c := check.Criteria{
		MaxFailed:    o.maxFailed,
		FirstAudio:   o.maxFirstAudio.limits,
		StopLatency:  o.maxStopLatency.limits,
		MinReplyRate: o.minReplyRate,
		MinStopRate:  o.minStopRate,
	}
	if o.failOn != "never" {
		c.FailOn = summary.Health(o.failOn)
	}
	return c
}

func (o *runOptions) validate(positional []string) error {
	switch {
	case len(positional) == 0:
		return usageErrorf("missing scenario file")
	case len(positional) > 1:
		return usageErrorf("expected one scenario file, got %d: %s", len(positional), strings.Join(positional, " "))
	case o.url == "":
		return usageErrorf("--url is required")
	case o.calls < 1:
		return usageErrorf("--calls must be at least 1")
	case o.concurrency < 1:
		return usageErrorf("--concurrency must be at least 1")
	case o.rampUp < 0:
		return usageErrorf("--ramp-up must not be negative")
	case o.callTimeout <= 0:
		return usageErrorf("--call-timeout must be positive")
	case o.stopWithin <= 0:
		return usageErrorf("--stop-within must be positive")
	case o.format != "text" && o.format != "json":
		return usageErrorf("--format must be text or json, got %q", o.format)
	case o.maxFailed < -1:
		return usageErrorf("--max-failed must be -1 or more")
	case o.failOn != "never" && o.failOn != string(summary.Degraded) && o.failOn != string(summary.Unreliable):
		return usageErrorf("--fail-on must be degraded, unreliable or never, got %q", o.failOn)
	case o.minReplyRate < 0 || o.minReplyRate > 1:
		return usageErrorf("--min-reply-rate must be between 0 and 1")
	case o.minStopRate < 0 || o.minStopRate > 1:
		return usageErrorf("--min-stop-rate must be between 0 and 1")
	}
	u, err := url.Parse(o.url)
	if err != nil {
		return usageErrorf("--url: %v", err)
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http", "https":
		return usageErrorf("--url must be a WebSocket URL; did you mean %s?", "ws"+strings.TrimPrefix(o.url, "http"))
	default:
		return usageErrorf("--url must start with ws:// or wss://, got %q", report.RedactURL(o.url))
	}
	if o.concurrency > o.calls {
		o.concurrency = o.calls
	}
	return nil
}

func describe(sc *scenario.Scenario) string {
	if sc.Name != "" {
		return fmt.Sprintf("%q (%d steps)", sc.Name, len(sc.Steps))
	}
	return fmt.Sprintf("scenario (%d steps)", len(sc.Steps))
}

// progress prints a line every progressInterval until stopped.
func progress(w io.Writer, total int, done, failed *atomic.Int64) (stop func()) {
	ticker := time.NewTicker(progressInterval)
	quit := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ticker.C:
				_, _ = fmt.Fprintf(w, "  %d/%d calls finished, %d failed\n", done.Load(), total, failed.Load())
			case <-quit:
				return
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(quit)
		wg.Wait()
	}
}

func writeFile(path string, doc report.Document) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := report.WriteJSON(f, doc); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
