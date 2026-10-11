package report

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ErenCanYildirim/callswarm/check"
	"github.com/ErenCanYildirim/callswarm/summary"
)

// WriteText writes a summary for people to read in a terminal or CI log.
// It is plain ASCII with no color, so it reads the same everywhere.
func WriteText(w io.Writer, s summary.Summary, m Manifest) error {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	name := m.Scenario
	if name == "" {
		name = "(unnamed scenario)"
	}
	p("callswarm %s - %s\n", m.Version, name)
	target := m.URL
	if m.Transport == "livekit" {
		target += " (LiveKit)"
	}
	p("%d calls, peak %d concurrent, %s against %s\n", s.Calls, s.MaxActive, human(s.Duration), target)
	p("health: %s\n", strings.ToUpper(string(s.Health)))

	p("\nTime to first audio\n")
	if s.FirstAudio.Count == 0 {
		p("  no replies to measure (%d turns played)\n", s.Turns)
	} else {
		p("  %d replies to %d turns", s.Replied, s.Turns)
		if s.Overlaps > 0 {
			p(", %d started before the caller finished", s.Overlaps)
		}
		p("\n\n")
		p("  %-14s %8s %8s %8s %8s %8s %6s\n", "calls active", "p50", "p90", "p95", "p99", "max", "n")
		row := func(label string, d summary.Distribution) {
			p("  %-14s %8s %8s %8s %8s %8s %6d\n", label,
				short(d.P50), short(d.P90), short(d.P95), short(d.P99), short(d.Max), d.Count)
		}
		row("all", s.FirstAudio)
		if len(s.ByConcurrency) > 1 {
			for _, bk := range s.ByConcurrency {
				label := fmt.Sprintf("%d-%d", bk.MinActive, bk.MaxActive)
				if bk.MinActive == bk.MaxActive {
					label = fmt.Sprint(bk.MinActive)
				}
				row(label, bk.FirstAudio)
			}
		}
	}

	if in := s.Interruptions; in.Count > 0 {
		p("\nInterruptions\n")
		p("  %s: stopped within %s in %d (%s)",
			plural(in.Count, "interruption"), short(time.Duration(m.StopWithinMS*float64(time.Millisecond))),
			in.Stopped, percent(in.Stopped, in.Count))
		// LiveKit has no clear message: an agent stops by stopping sending.
		if m.Transport != "livekit" {
			p(", sent clear in %d (%s)", in.Cleared, percent(in.Cleared, in.Count))
		}
		p("\n")
		if d := in.StopLatency; d.Count > 0 {
			p("  time talking over the caller: p50 %s, p90 %s, max %s (n=%d)\n",
				short(d.P50), short(d.P90), short(d.Max), d.Count)
		}
	}

	pc := s.Pacing
	p("\nCaller pacing\n")
	p("  %.2f%% of %d frames sent late, mean lateness %s, worst %s\n",
		pc.LateRatio*100, pc.FramesSent, short(pc.MeanLateness), short(pc.MaxLateness))

	if len(s.Warnings) > 0 {
		p("\nWarnings\n")
		for _, warn := range s.Warnings {
			p("  - %s\n", warn)
		}
	}

	p("\nscenario %s\n", m.ScenarioDigest)
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteChecks writes check results as a table, failures marked.
func WriteChecks(w io.Writer, results []check.Result) error {
	if len(results) == 0 {
		return nil
	}
	var b strings.Builder
	failed := check.Failed(results)
	if failed == 0 {
		fmt.Fprintf(&b, "\nChecks: all %d passed\n", len(results))
	} else {
		fmt.Fprintf(&b, "\nChecks: %d of %d failed\n", failed, len(results))
	}
	width := 0
	for _, r := range results {
		width = max(width, len(r.Name))
	}
	for _, r := range results {
		mark := "ok  "
		if !r.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  %s  %-*s  %s, want %s\n", mark, width, r.Name, r.Actual, r.Limit)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// short formats a latency compactly: "512ms", "1.25s", "0.6ms".
func short(d time.Duration) string {
	switch {
	case d < 0:
		return "-" + short(-d)
	case d < 10*time.Millisecond && d > 0:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Round(time.Millisecond).Milliseconds())
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// human formats a run length: "45.3s", "2m05s".
func human(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func percent(n, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", n*100/total)
}
