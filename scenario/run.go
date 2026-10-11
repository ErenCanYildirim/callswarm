package scenario

import (
	"context"
	"fmt"
	"time"

	"github.com/ErenCanYildirim/callswarm/timeline"
	"github.com/ErenCanYildirim/callswarm/transport"
)

// pollInterval is how often waits re-check the agent's audio. Recorded times
// come from the audio itself, so this affects only when the next step
// starts, not what is measured.
const pollInterval = 5 * time.Millisecond

// Result is the record of one run.
type Result struct {
	Scenario string
	Started  time.Time
	Ended    time.Time
	Steps    []StepResult
	// Agent is everything the agent sent, for computing metrics.
	Agent AgentAudio
	// Timeline is the run's event log
	Timeline *timeline.Timeline
	// Err is the error that stopped the run, if any. Steps after a failed
	// step are not run.
	Err error
}

// StepResult records what happened during one step.
type StepResult struct {
	Index int // from 1, matching the scenario file
	Kind  Kind
	Clip  string

	Started time.Time
	Ended   time.Time

	// Caller is the caller's audio, for say and interrupt.
	Caller transport.Playback
	// Reply is the agent speech the step waited for: the reply to a say or
	// listen, or the speech that an interrupt talked over (as it ended up,
	// so its End shows when the agent stopped). Nil if there was none.
	Reply *Segment

	Err error
}

// Run plays the scenario on an open call and returns what happened. It takes
// over the call's Inbound channel. The call is left open; the caller closes
// it.
func Run(ctx context.Context, conn *transport.Conn, sc *Scenario) *Result {
	r := &runner{ctx: ctx, conn: conn, sc: sc, mon: newMonitor(conn.Inbound())}
	res := &Result{Scenario: sc.Name, Started: time.Now()}
	for i, st := range sc.Steps {
		sr := StepResult{Index: i + 1, Kind: st.Kind, Clip: st.Clip, Started: time.Now()}
		sr.Err = r.step(st, &sr)
		sr.Ended = time.Now()
		res.Steps = append(res.Steps, sr)
		if sr.Err != nil {
			res.Err = fmt.Errorf("step %d (%s): %w", sr.Index, st.Kind, sr.Err)
			break
		}
	}
	res.Ended = time.Now()
	// Interrupted segments keep changing until the agent stops; take the
	// final view of each.
	for i := range res.Steps {
		if s := res.Steps[i].Reply; s != nil {
			final := r.mon.segmentAt(s.Start, sc.ReplyGap)
			res.Steps[i].Reply = &final
		}
	}
	res.Agent = r.mon.snapshot()
	res.Timeline = buildTimeline(sc, res)
	return res
}

type runner struct {
	ctx  context.Context
	conn *transport.Conn
	sc   *Scenario
	mon  *monitor
}

func (r *runner) step(st Step, sr *StepResult) error {
	switch st.Kind {
	case KindPause:
		return r.sleepUntil(time.Now().Add(st.Pause))

	case KindListen:
		seg, err := r.waitReply(sr.Started)
		sr.Reply = seg
		return err

	case KindSay:
		pb, err := r.conn.Play(r.ctx, st.Audio)
		sr.Caller = pb
		if err != nil {
			return err
		}
		return r.afterSpeaking(st, pb, sr)

	case KindInterrupt:
		seg, err := r.waitActive(sr.Started)
		if err != nil {
			return err
		}
		// Play starts the audio on the next frame boundary, so the caller's
		// speech begins at the requested offset or up to one frame after it.
		if err := r.sleepUntil(seg.Start.Add(st.After)); err != nil {
			return err
		}
		if now := r.mon.segmentAt(seg.Start, r.sc.ReplyGap); !now.End.After(time.Now()) {
			return fmt.Errorf("agent stopped speaking after %v, before the %v interruption point",
				now.End.Sub(now.Start).Round(time.Millisecond), st.After)
		}
		sr.Reply = &seg
		pb, err := r.conn.Play(r.ctx, st.Audio)
		sr.Caller = pb
		if err != nil {
			return err
		}
		return r.afterSpeaking(st, pb, sr)
	}
	return fmt.Errorf("unknown step kind %q", st.Kind)
}

func (r *runner) afterSpeaking(st Step, pb transport.Playback, sr *StepResult) error {
	switch st.Wait {
	case WaitReply:
		seg, err := r.waitReply(pb.Began)
		if st.Kind == KindSay {
			sr.Reply = seg
		}
		return err
	case WaitQuiet:
		return r.waitQuiet(pb.Ended)
	case WaitFixed:
		return r.sleepUntil(time.Now().Add(st.WaitFor))
	}
	return nil
}

// waitActive waits for agent speech still playing at, or starting after, t.
func (r *runner) waitActive(t time.Time) (Segment, error) {
	deadline := time.Now().Add(r.sc.ReplyTimeout)
	for {
		if seg, ok := r.mon.active(t, r.sc.ReplyGap); ok {
			return seg, nil
		}
		if time.Now().After(deadline) {
			return Segment{}, fmt.Errorf("no agent speech within %v", r.sc.ReplyTimeout)
		}
		if err := r.tick(); err != nil {
			return Segment{}, err
		}
	}
}

// waitReply waits for agent speech after t, then for it to finish.
func (r *runner) waitReply(t time.Time) (*Segment, error) {
	seg, err := r.waitActive(t)
	if err != nil {
		return nil, err
	}
	for {
		seg = r.mon.segmentAt(seg.Start, r.sc.ReplyGap)
		now := time.Now()
		if !now.Before(seg.End.Add(r.sc.ReplyGap)) {
			return &seg, nil
		}
		if now.Sub(seg.Start) > r.sc.MaxReply {
			return &seg, fmt.Errorf("agent reply still going after %v", r.sc.MaxReply)
		}
		if err := r.tick(); err != nil {
			return &seg, err
		}
	}
}

// waitQuiet waits until the agent has been silent for the reply gap, counting
// from no earlier than t.
func (r *runner) waitQuiet(t time.Time) error {
	start := time.Now()
	for {
		from := t
		if end := r.mon.lastSpeechEnd(); end.After(from) {
			from = end
		}
		now := time.Now()
		if !now.Before(from.Add(r.sc.ReplyGap)) {
			return nil
		}
		if now.Sub(start) > r.sc.MaxReply {
			return fmt.Errorf("agent still talking after %v", r.sc.MaxReply)
		}
		if err := r.tick(); err != nil {
			return err
		}
	}
}

// tick waits one poll interval, failing if the run or the call has ended.
func (r *runner) tick() error {
	return r.sleepUntil(time.Now().Add(pollInterval))
}

func (r *runner) sleepUntil(t time.Time) error {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-r.conn.Done():
		return fmt.Errorf("call ended: %w", r.conn.Err())
	}
}
