// Command jargo-bot is the Jargo phone agent for the callswarm benchmark.
//
// It speaks the Twilio Media Streams protocol on ws://ADDR/ws, as Twilio
// would connect to it, and runs Deepgram speech-to-text, Gemini and
// ElevenLabs text-to-speech with Silero VAD and Smart Turn v3 for turn
// taking. It is built to match the Pipecat bot in ../pipecat setting for
// setting; it is adapted from Jargo's examples/twiliobot.
//
// Run:  go run . [-addr 127.0.0.1:8765]
// Needs DEEPGRAM_API_KEY, GEMINI_API_KEY and ELEVENLABS_API_KEY, and the ONNX
// Runtime shared library for turn taking (JARGO_ONNXRUNTIME_LIB).
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/gojargo/jargo/audio/turn"
	"github.com/gojargo/jargo/audio/vad"
	"github.com/gojargo/jargo/frames"
	"github.com/gojargo/jargo/pipeline"
	"github.com/gojargo/jargo/processor"
	"github.com/gojargo/jargo/processor/aggregators"
	"github.com/gojargo/jargo/processor/turns"
	"github.com/gojargo/jargo/processor/vadproc"
	"github.com/gojargo/jargo/provider/deepgram"
	"github.com/gojargo/jargo/provider/elevenlabs"
	"github.com/gojargo/jargo/provider/google/gemini"
	"github.com/gojargo/jargo/transport/wsserver"
	"github.com/gojargo/jargo/transport/wsserver/twilio"
)

// Shared with the Pipecat bot. Change both together, or the comparison is off.
const (
	sampleRate = 16000
	sttModel   = "nova-3-general"
	llmModel   = "gemini-3.6-flash"
	ttsModel   = "eleven_flash_v2_5"
	ttsVoice   = "21m00Tcm4TlvDq8ikWAM"
	maxTokens  = 1024

	systemPrompt = "You are a friendly voice assistant for a small bakery on a phone call. " +
		"The bakery is open Monday to Saturday from 7am to 6pm and closed on Sunday. " +
		"Keep your replies short, warm and conversational: one or two sentences."
	greeting = "Hello! Thanks for calling the bakery. How can I help you today?"
)

type keys struct{ deepgram, gemini, elevenlabs string }

func main() {
	addr := flag.String("addr", "127.0.0.1:8765", "address to listen on")
	flag.Parse()

	k := keys{
		deepgram:   mustEnv("DEEPGRAM_API_KEY"),
		gemini:     mustEnv("GEMINI_API_KEY"),
		elevenlabs: mustEnv("ELEVENLABS_API_KEY"),
	}
	// Turn taking is part of what is being compared, so the bot refuses to
	// run without it rather than quietly falling back to STT endpointing,
	// which also loses barge-in.
	if err := checkTurnTaking(); err != nil {
		log.Fatalf("turn taking unavailable (set JARGO_ONNXRUNTIME_LIB to the ONNX Runtime library): %v", err)
	}

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) { handleStream(w, r, k) })
	slog.Info("jargo bot listening", "url", "ws://"+*addr+"/ws")
	log.Fatal(http.ListenAndServe(*addr, nil)) //nolint:gosec // local benchmark server
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is not set", name)
	}
	return v
}

func checkTurnTaking() error {
	vd, err := vad.NewSilero()
	if err != nil {
		return err
	}
	_ = vd.Close()
	tr, err := turn.NewSmartTurnV3()
	if err != nil {
		return err
	}
	_ = tr.Close()
	return nil
}

func handleStream(w http.ResponseWriter, r *http.Request, k keys) {
	// Hanging up is Twilio's REST API's job, and a test caller has no Twilio
	// account. Left on, the serializer refuses to start without credentials.
	off := false
	ser := twilio.New(twilio.Config{AutoHangUp: &off})

	params := wsserver.DefaultParams()
	params.AudioInSampleRate = sampleRate
	params.AudioOutSampleRate = sampleRate
	t, err := wsserver.Accept(w, r, ser, params)
	if err != nil {
		slog.Error("accept websocket", "err", err)
		return
	}
	if err := runBot(t, k); err != nil {
		slog.Error("call ended with an error", "err", err)
	}
}

func runBot(t *wsserver.Transport, k keys) error {
	vd, err := vad.NewSilero()
	if err != nil {
		return err
	}
	tr, err := turn.NewSmartTurnV3()
	if err != nil {
		_ = vd.Close()
		return err
	}

	stt := deepgram.NewSTT(deepgram.Config{APIKey: k.deepgram, Model: sttModel})
	llm := gemini.NewLLM(gemini.Config{APIKey: k.gemini, Model: llmModel, MaxTokens: maxTokens})
	tts := elevenlabs.NewTTS(elevenlabs.Config{
		APIKey:     k.elevenlabs,
		Model:      ttsModel,
		VoiceID:    ttsVoice,
		SampleRate: sampleRate,
	})

	convo := frames.NewLLMContext(systemPrompt)
	agg := aggregators.New(convo, aggregators.WithTurns(turns.Config{
		Strategies: turns.UserTurnStrategies{
			Start: turns.DefaultStartStrategies(),
			Stop:  []turns.StopStrategy{turns.NewTurnAnalyzerStop(turns.TurnAnalyzerConfig{Analyzer: tr})},
		},
	}))
	procs := []processor.Processor{
		t.Input(),
		vadproc.New(vadproc.Config{VAD: vd}),
		stt,
		agg.User(),
		llm,
		tts,
		t.Output(),
		agg.Assistant(),
	}
	task := pipeline.NewWorker(pipeline.New(procs...), pipeline.WorkerConfig{
		Params: pipeline.Params{AudioInSampleRate: sampleRate, AudioOutSampleRate: sampleRate},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-t.Done() // the caller hung up
		cancel()
	}()

	task.QueueFrame(frames.NewTTSSpeakFrame(greeting))
	slog.Info("call started")
	if err := task.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	slog.Info("call ended")
	return nil
}
