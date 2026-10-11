// Package load runs a scenario as many concurrent calls against an agent.
//
// A Plan says how many calls to make in total and how many may be active at
// once. Concurrency ramps up linearly: with Concurrency 10 and RampUp 9s, one
// call may run at the start, two after 1 s, and all ten after 9 s. As calls
// finish, new ones start in their place until all have been made.
//
// Every call has its own timeout. Cancelling the context stops new calls from
// starting and winds down the ones in progress; the report then covers the
// calls that started.
package load

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ErenCanYildirim/callswarm/metrics"
	"github.com/ErenCanYildirim/callswarm/scenario"
	"github.com/ErenCanYildirim/callswarm/transport"
)

// DefaultCallTimeout bounds a call when the plan doesn't set a timeout.
const DefaultCallTimeout = 5 * time.Minute

// Plan describes a load run.
type Plan struct {
	// URL is the agent's Twilio Media Streams WebSocket endpoint.
	URL string
	// Scenario is what every caller does.
	Scenario *scenario.Scenario
	// Calls is the total number of calls to make.
	Calls int
	// Concurrency is the most calls active at once.
	Concurrency int
	// RampUp is how long concurrency takes to grow from 1 to Concurrency.
	// Zero starts at full concurrency.
	RampUp time.Duration
	// CallTimeout bounds each call, from dialing to hanging up.
	// Default 5 minutes.
	CallTimeout time.Duration

	// Header and Parameters are passed to every call; see transport.Options.
	Header     http.Header
	Parameters map[string]string

	// Metrics tunes how each call is judged.
	Metrics metrics.Options

	// KeepAgentAudio keeps the agent's audio in each call's result. It is
	// dropped by default, since it is about 8 KB per second per call; the
	// timeline and metrics are always kept.
	KeepAgentAudio bool

	// OnCall, if set, is called as each call finishes, for progress output.
	// It is called from multiple goroutines.
	OnCall func(Call)
}

// Call is the outcome of one call.
type Call struct {
	// ID numbers calls in the order they started, from 1.
	ID int
	// ActiveAtStart is how many calls were running when this one started,
	// including itself.
	ActiveAtStart int
	Started       time.Time
	Ended         time.Time
	// Result is the scenario run, or nil if the call never connected.
	Result *scenario.Result
	// Metrics is computed from Result.
	Metrics metrics.Call
	// Pacing is how well the caller kept real-time pace on this call.
	Pacing transport.Stats
	// Err is why the call failed: a dial error, the scenario's error, or a
	// timeout. Nil on success.
	Err error
}

// Report is the outcome of a run.
type Report struct {
	Plan    Plan
	Started time.Time
	Ended   time.Time
	// Calls are the calls that started, in start order.
	Calls []Call
	// MaxActive is the highest number of calls running at once.
	MaxActive int
	// Cancelled reports whether the run was stopped before all calls
	// were made.
	Cancelled bool
}

// Failed counts calls that ended with an error.
func (r *Report) Failed() int {
	n := 0
	for _, c := range r.Calls {
		if c.Err != nil {
			n++
		}
	}
	return n
}

// Validate checks a plan for mistakes.
func (p *Plan) Validate() error {
	switch {
	case p.URL == "":
		return errors.New("plan: URL is required")
	case p.Scenario == nil:
		return errors.New("plan: scenario is required")
	case p.Calls < 1:
		return fmt.Errorf("plan: calls must be at least 1, got %d", p.Calls)
	case p.Concurrency < 1:
		return fmt.Errorf("plan: concurrency must be at least 1, got %d", p.Concurrency)
	case p.RampUp < 0:
		return fmt.Errorf("plan: ramp-up must not be negative, got %v", p.RampUp)
	case p.CallTimeout < 0:
		return fmt.Errorf("plan: call timeout must not be negative, got %v", p.CallTimeout)
	}
	return nil
}

// allowed is how many calls may be active at elapsed time t.
func (p *Plan) allowed(t time.Duration) int {
	if p.RampUp == 0 || p.Concurrency == 1 || t >= p.RampUp {
		return p.Concurrency
	}
	return 1 + int(int64(p.Concurrency-1)*int64(t)/int64(p.RampUp))
}

// nextStep is when the allowed concurrency next increases, as elapsed time.
func (p *Plan) nextStep(current int) time.Duration {
	// allowed(t) reaches current+1 when (C-1)*t/R >= current.
	steps := int64(p.Concurrency - 1)
	return time.Duration((int64(current)*int64(p.RampUp) + steps - 1) / steps)
}

// Run executes the plan. It returns an error only for an invalid plan;
// failed calls are recorded in the report. When ctx is cancelled, Run stops
// starting calls, cancels the calls in progress, waits for them to hang up,
// and returns the partial report.
func Run(ctx context.Context, p Plan) (*Report, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.CallTimeout == 0 {
		p.CallTimeout = DefaultCallTimeout
	}

	rep := &Report{Plan: p, Started: time.Now()}
	var mu sync.Mutex // guards rep.Calls
	finished := make(chan struct{}, p.Calls)
	var wg sync.WaitGroup
	active := 0

schedule:
	for id := 1; id <= p.Calls; id++ {
		for {
			if ctx.Err() != nil {
				rep.Cancelled = true
				break schedule
			}
			allowed := p.allowed(time.Since(rep.Started))
			if active < allowed {
				break
			}
			// Wait for a call to finish, or for the ramp to allow another.
			var rampTick <-chan time.Time
			var timer *time.Timer
			if allowed < p.Concurrency {
				timer = time.NewTimer(p.nextStep(allowed) - time.Since(rep.Started))
				rampTick = timer.C
			}
			select {
			case <-finished:
				active--
			case <-rampTick:
			case <-ctx.Done():
			}
			if timer != nil {
				timer.Stop()
			}
		}

		active++
		rep.MaxActive = max(rep.MaxActive, active)
		c := Call{ID: id, ActiveAtStart: active}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := runCall(ctx, &p, c)
			mu.Lock()
			rep.Calls = append(rep.Calls, c)
			mu.Unlock()
			if p.OnCall != nil {
				p.OnCall(c)
			}
			finished <- struct{}{}
		}()
	}

	wg.Wait()
	rep.Ended = time.Now()
	// Calls cut short by cancellation count too, even if all had started.
	rep.Cancelled = rep.Cancelled || ctx.Err() != nil
	slices.SortFunc(rep.Calls, func(a, b Call) int { return a.ID - b.ID })
	return rep, nil
}

// runCall makes one call: dial, play the scenario, hang up.
func runCall(ctx context.Context, p *Plan, c Call) Call {
	c.Started = time.Now()
	ctx, cancel := context.WithTimeout(ctx, p.CallTimeout)
	defer cancel()

	conn, err := transport.Dial(ctx, p.URL, transport.Options{
		CallID:     fmt.Sprintf("%06d", c.ID),
		Header:     p.Header,
		Parameters: p.Parameters,
	})
	if err != nil {
		c.Err = err
		c.Ended = time.Now()
		return c
	}

	res := scenario.Run(ctx, conn, p.Scenario)
	c.Pacing = conn.Stats()
	closeErr := conn.Close()

	c.Metrics = metrics.FromResult(res, p.Metrics)
	if !p.KeepAgentAudio {
		res.Agent.Frames = nil
	}
	c.Result = res
	switch {
	case res.Err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		c.Err = fmt.Errorf("call timed out after %v: %w", p.CallTimeout, res.Err)
	case res.Err != nil:
		c.Err = res.Err
	case closeErr != nil:
		c.Err = closeErr
	}
	c.Ended = time.Now()
	return c
}
