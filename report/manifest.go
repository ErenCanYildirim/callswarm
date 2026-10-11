package report

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"runtime"
	"time"

	"github.com/ErenCanYildirim/callswarm/load"
	"github.com/ErenCanYildirim/callswarm/metrics"
	"github.com/ErenCanYildirim/callswarm/scenario"
	"github.com/ErenCanYildirim/callswarm/summary"
)

// Manifest records what produced a report, so two reports can be compared
// and a run can be repeated. Runs with the same scenario digest and plan
// settings are comparable; differences in machine fields explain
// differences in health.
type Manifest struct {
	Version   string `json:"callswarm_version"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`

	// URL is the agent endpoint with credentials and query removed.
	URL string `json:"url"`

	Scenario       string `json:"scenario"`
	ScenarioDigest string `json:"scenario_digest"`

	Calls         int            `json:"calls"`
	Concurrency   int            `json:"concurrency"`
	RampUpMS      float64        `json:"ramp_up_ms"`
	CallTimeoutMS float64        `json:"call_timeout_ms"`
	StopWithinMS  float64        `json:"stop_within_ms"`
	Thresholds    jsonThresholds `json:"thresholds"`
}

type jsonThresholds struct {
	WarnLateRatio       float64 `json:"warn_late_ratio"`
	UnreliableLateRatio float64 `json:"unreliable_late_ratio"`
	MinSamples          int     `json:"min_samples"`
}

// NewManifest describes a run. version is the callswarm version string.
func NewManifest(version string, p load.Plan, th summary.Thresholds) Manifest {
	th = th.WithDefaults()
	callTimeout := p.CallTimeout
	if callTimeout == 0 {
		callTimeout = load.DefaultCallTimeout
	}
	stopWithin := p.Metrics.StopWithin
	if stopWithin == 0 {
		stopWithin = metrics.DefaultStopWithin
	}
	m := Manifest{
		Version:       version,
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPUs:          runtime.NumCPU(),
		URL:           RedactURL(p.URL),
		Calls:         p.Calls,
		Concurrency:   p.Concurrency,
		RampUpMS:      ms(p.RampUp),
		CallTimeoutMS: ms(callTimeout),
		StopWithinMS:  ms(stopWithin),
		Thresholds: jsonThresholds{
			WarnLateRatio:       th.WarnLateRatio,
			UnreliableLateRatio: th.UnreliableLateRatio,
			MinSamples:          th.MinSamples,
		},
	}
	if p.Scenario != nil {
		m.Scenario = p.Scenario.Name
		m.ScenarioDigest = Digest(p.Scenario)
	}
	return m
}

// RedactURL removes user info and the query string, which commonly carry
// credentials.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// Digest identifies a scenario by what it does: its settings, its steps and
// the audio it plays, after conversion. Renaming a clip file doesn't change
// it; re-recording one does.
func Digest(sc *scenario.Scenario) string {
	h := sha256.New()
	num := func(v int64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v))
		h.Write(b[:])
	}
	bytes := func(b []byte) {
		num(int64(len(b)))
		h.Write(b)
	}
	num(int64(sc.ReplyTimeout))
	num(int64(sc.ReplyGap))
	num(int64(sc.MaxReply))
	for _, st := range sc.Steps {
		bytes([]byte(st.Kind))
		bytes([]byte(st.Wait))
		num(int64(st.Pause))
		num(int64(st.After))
		num(int64(st.WaitFor))
		bytes(st.Audio)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
