package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/ErenCanYildirim/callswarm/fakeagent"
)

func fakeAgentCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var addr string
	var cfg fakeagent.Config
	fs := flag.NewFlagSet("fake-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&addr, "addr", "127.0.0.1:8080", "address to listen on")
	fs.DurationVar(&cfg.ResponseDelay, "delay", 200*time.Millisecond, "pause after the caller stops before answering")
	fs.DurationVar(&cfg.EndpointSilence, "endpoint", 300*time.Millisecond, "silence that ends the caller's turn")
	fs.BoolVar(&cfg.BargeIn, "barge-in", true, "stop talking when the caller interrupts")
	fs.DurationVar(&cfg.BargeInDelay, "barge-in-delay", 0, "reaction time before stopping on interruption")
	fs.BoolVar(&cfg.Burst, "burst", false, "send each reply at once instead of in real time")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage: callswarm fake-agent [flags]\n\n"+
			"Serves an agent that echoes the caller back after a fixed delay, speaking\n"+
			"the Twilio Media Streams protocol on any path. Its time to first audio is\n"+
			"exactly --endpoint plus --delay, so callswarm's measurements of it can be\n"+
			"checked against a known answer.\n\nFlags:\n")
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
	return exitCode(serveFakeAgent(ctx, addr, cfg, stdout), "fake-agent", stderr)
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
