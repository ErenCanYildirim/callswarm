package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/fakeagent/livekitagent"
)

func fakeAgentCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var addr, mode, prefix string
	var srv livekitagent.Server
	var cfg fakeagent.Config
	fs := flag.NewFlagSet("fake-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&mode, "transport", "twilio", "twilio (serve Media Streams WebSockets) or livekit (join LiveKit rooms)")
	fs.StringVar(&addr, "addr", "127.0.0.1:8080", "twilio: address to listen on")
	fs.StringVar(&srv.URL, "url", "", "livekit: server URL (default $LIVEKIT_URL)")
	fs.StringVar(&srv.APIKey, "api-key", "", "livekit: API key (default $LIVEKIT_API_KEY)")
	fs.StringVar(&srv.APISecret, "api-secret", "", "livekit: API secret (default $LIVEKIT_API_SECRET)")
	fs.StringVar(&prefix, "room-prefix", "callswarm-", "livekit: join new rooms whose names start with this")
	fs.DurationVar(&cfg.ResponseDelay, "delay", 200*time.Millisecond, "pause after the caller stops before answering")
	fs.DurationVar(&cfg.EndpointSilence, "endpoint", 300*time.Millisecond, "silence that ends the caller's turn")
	fs.BoolVar(&cfg.BargeIn, "barge-in", true, "stop talking when the caller interrupts")
	fs.DurationVar(&cfg.BargeInDelay, "barge-in-delay", 0, "reaction time before stopping on interruption")
	fs.BoolVar(&cfg.Burst, "burst", false, "send each reply at once instead of in real time")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage: callswarm fake-agent [flags]\n\n"+
			"Serves an agent that echoes the caller back after a fixed delay, speaking\n"+
			"the Twilio Media Streams protocol on any path, or, with --transport livekit,\n"+
			"joining every new LiveKit room whose name has the --room-prefix. Its time\n"+
			"to first audio is exactly --endpoint plus --delay, so callswarm's\n"+
			"measurements of it can be checked against a known answer.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		return exitCode(usageErrorf("unexpected argument %q", fs.Arg(0)), "fake-agent", stderr)
	}
	switch mode {
	case "twilio":
		return exitCode(serveFakeAgent(ctx, addr, cfg, stdout), "fake-agent", stderr)
	case "livekit":
		return exitCode(serveLiveKitFakeAgent(ctx, srv, prefix, cfg, stdout, stderr), "fake-agent", stderr)
	}
	return exitCode(usageErrorf("--transport must be twilio or livekit, got %q", mode), "fake-agent", stderr)
}

func serveLiveKitFakeAgent(ctx context.Context, srv livekitagent.Server, prefix string, cfg fakeagent.Config, stdout, stderr io.Writer) error {
	if srv.URL == "" {
		srv.URL = os.Getenv("LIVEKIT_URL")
	}
	if srv.APIKey == "" {
		srv.APIKey = os.Getenv("LIVEKIT_API_KEY")
	}
	if srv.APISecret == "" {
		srv.APISecret = os.Getenv("LIVEKIT_API_SECRET")
	}
	switch {
	case srv.URL == "":
		return usageErrorf("--url or LIVEKIT_URL is required with --transport livekit")
	case srv.APIKey == "" || srv.APISecret == "":
		return usageErrorf("an API key and secret are required: set LIVEKIT_API_KEY and LIVEKIT_API_SECRET, or --api-key and --api-secret")
	}
	quietLiveKit()
	_, _ = fmt.Fprintf(stdout, "fake agent joining rooms named %s* on %s (first audio %v after caller speech ends, barge-in %v)\n",
		prefix, srv.URL, cfg.ExpectedFirstAudio(), cfg.BargeIn)
	logf := func(format string, args ...any) { _, _ = fmt.Fprintf(stderr, format+"\n", args...) }
	return livekitagent.Serve(ctx, srv, prefix, cfg, logf)
}

func serveFakeAgent(ctx context.Context, addr string, cfg fakeagent.Config, stdout io.Writer) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: fakeagent.New(cfg), ReadHeaderTimeout: 10 * time.Second}
	_, _ = fmt.Fprintf(stdout, "fake agent listening on ws://%s (first audio %v after caller speech ends, barge-in %v)\n",
		ln.Addr(), cfg.ExpectedFirstAudio(), cfg.BargeIn)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// WebSocket connections are hijacked and not tracked by Shutdown;
		// closing the listener is what matters.
		_ = srv.Shutdown(shutdown)
		return nil
	}
}
