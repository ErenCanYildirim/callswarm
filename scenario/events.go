package scenario

import (
	"fmt"
	"time"

	"github.com/ErenCanYildirim/callswarm/timeline"
)

// buildTimeline turns a finished run into its event log.
func buildTimeline(sc *Scenario, res *Result) *timeline.Timeline {
	tl := timeline.New(res.Started)
	tl.Add(res.Started, timeline.RunStarted, 0, sc.Name)

	for _, sr := range res.Steps {
		st := sc.Steps[sr.Index-1]
		tl.Add(sr.Started, timeline.StepStarted, sr.Index, describe(st))
		if sr.Caller.Frames > 0 {
			addCallerAudio(tl, st, sr)
		}
		detail := ""
		if sr.Err != nil {
			detail = sr.Err.Error()
		}
		tl.Add(sr.Ended, timeline.StepEnded, sr.Index, detail)
	}

	// Agent events are attributed to the step that was running when they
	// happened, which is what you want when reading the log; metrics match
	// replies to steps explicitly instead.
	stepAt := func(t time.Time) int {
		n := 0
		for _, sr := range res.Steps {
			if !sr.Started.After(t) {
				n = sr.Index
			}
		}
		return n
	}
	for _, seg := range res.Agent.Speech(vadConfig.MinSilence) {
		tl.Add(seg.Start, timeline.AgentAudioStarted, stepAt(seg.Start), "")
		detail := ""
		for _, c := range res.Agent.Clears {
			if c.Equal(seg.End) {
				detail = "cut off by clear"
			}
		}
		tl.Add(seg.End, timeline.AgentAudioStopped, stepAt(seg.End), detail)
	}
	for _, c := range res.Agent.Clears {
		tl.Add(c, timeline.AgentClear, stepAt(c), "")
	}
	for _, m := range res.Agent.Marks {
		tl.Add(m.At, timeline.AgentMark, stepAt(m.At), m.Name)
	}

	if res.Err != nil {
		tl.Add(res.Ended, timeline.RunFailed, 0, res.Err.Error())
	}
	tl.Add(res.Ended, timeline.RunEnded, 0, "")
	tl.Sort()
	return tl
}

// addCallerAudio records the clip as sent and the speech within it. If the
// clip was cut short, only the speech that was actually sent is recorded.
func addCallerAudio(tl *timeline.Timeline, st Step, sr StepResult) {
	pb := sr.Caller
	tl.Add(pb.Began, timeline.CallerAudioStarted, sr.Index, "")
	for _, sp := range st.Speech {
		start := pb.Began.Add(sp.Start)
		if !start.Before(pb.Ended) {
			break
		}
		end := pb.Began.Add(sp.End)
		if end.After(pb.Ended) {
			end = pb.Ended
		}
		tl.Add(start, timeline.CallerSpeechStarted, sr.Index, "")
		tl.Add(end, timeline.CallerSpeechEnded, sr.Index, "")
	}
	tl.Add(pb.Ended, timeline.CallerAudioEnded, sr.Index, "")
}

func describe(st Step) string {
	switch st.Kind {
	case KindSay:
		return "say " + st.Clip
	case KindInterrupt:
		return fmt.Sprintf("interrupt %s after %v", st.Clip, st.After)
	case KindPause:
		return fmt.Sprintf("pause %v", st.Pause)
	}
	return string(st.Kind)
}
