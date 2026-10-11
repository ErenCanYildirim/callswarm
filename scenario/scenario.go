// Package scenario defines scripted caller conversations and runs them
// against a voice agent.
//
// A scenario is a YAML file listing what the caller does, in order:
//
//	name: book-appointment
//	reply_timeout: 10s   # longest wait for the agent to start speaking
//	reply_gap: 700ms     # silence that ends an agent reply
//	steps:
//	  - listen                     # wait for the agent's greeting
//	  - say: clips/hello.wav       # speak, then wait for the reply
//	  - pause: 1s                  # stay silent
//	  - say: clips/question.wav
//	    wait: none                 # don't wait for the reply...
//	  - interrupt: clips/stop.wav  # ...talk over it instead,
//	    after: 800ms               # 800 ms after the agent starts speaking
//
// Clip paths are relative to the scenario file. Clips are WAV files with
// 16-bit samples at any rate; they are converted to 8 kHz μ-law on load.
//
// The wait option controls what happens after say and interrupt:
//
//	reply  wait for the agent to start a reply and finish it (default for say)
//	quiet  wait until the agent has stopped talking (default for interrupt)
//	none   continue immediately
//	<dur>  wait a fixed time, such as 2s
package scenario

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/vad"
)

// Defaults for scenario-wide settings.
const (
	DefaultReplyTimeout = 10 * time.Second
	DefaultReplyGap     = 700 * time.Millisecond
	DefaultMaxReply     = 60 * time.Second
)

// Kind is what a step does.
type Kind string

// Step kinds.
const (
	KindSay       Kind = "say"
	KindPause     Kind = "pause"
	KindListen    Kind = "listen"
	KindInterrupt Kind = "interrupt"
)

// WaitMode is what the caller waits for after speaking.
type WaitMode string

// Wait modes.
const (
	WaitReply WaitMode = "reply"
	WaitQuiet WaitMode = "quiet"
	WaitNone  WaitMode = "none"
	WaitFixed WaitMode = "fixed" // wait Step.WaitFor
)

// Scenario is a parsed, validated scenario with its audio loaded.
type Scenario struct {
	Name string
	// ReplyTimeout is the longest the caller waits for the agent to start
	// speaking before the step fails.
	ReplyTimeout time.Duration
	// ReplyGap is how long the agent must be silent for its reply to count
	// as finished. It must be longer than the pauses within a reply.
	ReplyGap time.Duration
	// MaxReply is the longest a single agent reply may run.
	MaxReply time.Duration
	Steps    []Step
}

// Step is one caller action.
type Step struct {
	Kind Kind
	// Clip is the clip path as written in the file, for say and interrupt.
	Clip string
	// Audio is the clip as 8 kHz μ-law, for say and interrupt.
	Audio []byte
	// Speech is where the clip contains speech, found by voice activity
	// detection. Recordings usually have silence before and after the
	// words; caller speech ends where the words end, not the file.
	Speech []vad.Span
	// Pause is the silence length, for pause.
	Pause time.Duration
	// After is how long after the agent starts speaking to interrupt.
	After time.Duration
	// Wait is what to wait for after a say or interrupt.
	Wait WaitMode
	// WaitFor is the fixed wait when Wait is WaitFixed.
	WaitFor time.Duration
}

// file is the YAML shape of a scenario.
type file struct {
	Name         string     `yaml:"name"`
	ReplyTimeout string     `yaml:"reply_timeout"`
	ReplyGap     string     `yaml:"reply_gap"`
	MaxReply     string     `yaml:"max_reply"`
	Steps        []fileStep `yaml:"steps"`
}

// fileStep is one step as written: either the bare word "listen" or a map
// with exactly one action key.
type fileStep struct {
	Listen    bool
	Say       string `yaml:"say"`
	Interrupt string `yaml:"interrupt"`
	Pause     string `yaml:"pause"`
	Wait      string `yaml:"wait"`
	After     string `yaml:"after"`
}

// stepMap is fileStep without its custom unmarshaler, so decoding a map
// doesn't recurse.
type stepMap struct {
	Say       string `yaml:"say"`
	Interrupt string `yaml:"interrupt"`
	Pause     string `yaml:"pause"`
	Wait      string `yaml:"wait"`
	After     string `yaml:"after"`
}

// UnmarshalYAML accepts "listen" or a map of step fields.
func (s *fileStep) UnmarshalYAML(unmarshal func(any) error) error {
	var word string
	if err := unmarshal(&word); err == nil {
		if word != string(KindListen) {
			return fmt.Errorf("unknown step %q (the only bare step is %q)", word, KindListen)
		}
		s.Listen = true
		return nil
	}
	var m stepMap
	if err := unmarshal(&m); err != nil {
		return err
	}
	*s = fileStep{Say: m.Say, Interrupt: m.Interrupt, Pause: m.Pause, Wait: m.Wait, After: m.After}
	return nil
}

// Load reads a scenario file and the clips it references.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sc, err := Parse(data, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sc, nil
}

// Parse decodes and validates a scenario. Clip paths are resolved against
// baseDir and loaded.
func Parse(data []byte, baseDir string) (*Scenario, error) {
	var f file
	if err := yaml.UnmarshalWithOptions(data, &f, yaml.DisallowUnknownField()); err != nil {
		return nil, errors.New(yaml.FormatError(err, false, true))
	}
	sc := &Scenario{Name: f.Name}
	var err error
	if sc.ReplyTimeout, err = durationOr(f.ReplyTimeout, DefaultReplyTimeout, "reply_timeout"); err != nil {
		return nil, err
	}
	if sc.ReplyGap, err = durationOr(f.ReplyGap, DefaultReplyGap, "reply_gap"); err != nil {
		return nil, err
	}
	if sc.MaxReply, err = durationOr(f.MaxReply, DefaultMaxReply, "max_reply"); err != nil {
		return nil, err
	}
	if len(f.Steps) == 0 {
		return nil, errors.New("scenario has no steps")
	}

	clips := map[string]clip{}
	for i, fs := range f.Steps {
		st, err := compileStep(fs, baseDir, clips)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		sc.Steps = append(sc.Steps, st)
	}
	return sc, nil
}

func compileStep(fs fileStep, baseDir string, clips map[string]clip) (Step, error) {
	var actions []Kind
	if fs.Listen {
		actions = append(actions, KindListen)
	}
	if fs.Say != "" {
		actions = append(actions, KindSay)
	}
	if fs.Interrupt != "" {
		actions = append(actions, KindInterrupt)
	}
	if fs.Pause != "" {
		actions = append(actions, KindPause)
	}
	if len(actions) != 1 {
		return Step{}, fmt.Errorf("need exactly one of say, interrupt, pause or listen, got %d", len(actions))
	}
	st := Step{Kind: actions[0]}

	if fs.After != "" && st.Kind != KindInterrupt {
		return Step{}, errors.New("after is only valid on interrupt")
	}
	if fs.Wait != "" && st.Kind != KindSay && st.Kind != KindInterrupt {
		return Step{}, errors.New("wait is only valid on say and interrupt")
	}

	var err error
	switch st.Kind {
	case KindPause:
		if st.Pause, err = positiveDuration(fs.Pause, "pause"); err != nil {
			return Step{}, err
		}
	case KindSay, KindInterrupt:
		st.Clip = fs.Say + fs.Interrupt
		c, err := loadClip(st.Clip, baseDir, clips)
		if err != nil {
			return Step{}, err
		}
		st.Audio, st.Speech = c.audio, c.speech
		st.Wait = WaitReply
		if st.Kind == KindInterrupt {
			st.Wait = WaitQuiet
			if fs.After == "" {
				return Step{}, errors.New("interrupt needs after, such as after: 800ms")
			}
			if st.After, err = time.ParseDuration(fs.After); err != nil || st.After < 0 {
				return Step{}, fmt.Errorf("after: invalid duration %q", fs.After)
			}
		}
		if fs.Wait != "" {
			if st.Wait, st.WaitFor, err = parseWait(fs.Wait); err != nil {
				return Step{}, err
			}
		}
	}
	return st, nil
}

func parseWait(s string) (WaitMode, time.Duration, error) {
	switch WaitMode(s) {
	case WaitReply, WaitQuiet, WaitNone:
		return WaitMode(s), 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return "", 0, fmt.Errorf("wait: %q is not reply, quiet, none or a duration", s)
	}
	return WaitFixed, d, nil
}

// clip is a loaded clip in telephony format.
type clip struct {
	audio  []byte
	speech []vad.Span
}

// loadClip reads a WAV clip once per path, converts it to telephony format
// and finds the speech in it.
func loadClip(name, baseDir string, cache map[string]clip) (clip, error) {
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, filepath.FromSlash(name))
	}
	if c, ok := cache[path]; ok {
		return c, nil
	}
	if !strings.EqualFold(filepath.Ext(path), ".wav") {
		return clip{}, fmt.Errorf("clip %s: only .wav files are supported", name)
	}
	pcm, err := audio.LoadWAV(path)
	if err != nil {
		return clip{}, fmt.Errorf("clip %s: %w", name, err)
	}
	pcm, err = audio.Resample(pcm, audio.TelephonySampleRate)
	if err != nil {
		return clip{}, fmt.Errorf("clip %s: %w", name, err)
	}
	if len(pcm.Samples) == 0 {
		return clip{}, fmt.Errorf("clip %s: no audio", name)
	}
	c := clip{audio: audio.MulawEncode(pcm.Samples), speech: vad.Detect(pcm.Samples, vad.Config{})}
	if len(c.speech) == 0 {
		return clip{}, fmt.Errorf("clip %s: no speech detected (too quiet?)", name)
	}
	cache[path] = c
	return c, nil
}

func durationOr(s string, def time.Duration, field string) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return positiveDuration(s, field)
}

func positiveDuration(s, field string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid duration %q", field, s)
	}
	return d, nil
}
