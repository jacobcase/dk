package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// signalContext returns a context cancelled on SIGINT or SIGTERM, so an
// in-flight DigiKey request is abandoned promptly on Ctrl-C.
//
// It deliberately does not use signal.NotifyContext, which keeps the handler
// installed after the first signal and so swallows every later one. Commands
// that block somewhere the context cannot reach — a read from stdin in
// `dk auth login --manual`, or `dk list add --from-json -` — would then be
// unkillable with Ctrl-C. Here the trap is released as soon as it fires, so a
// second Ctrl-C hits Go's default handler and terminates the process.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	go func() {
		// Either branch stops the goroutine, so it cannot outlive the context.
		select {
		case <-ch:
			signal.Stop(ch)
			cancel()
		case <-ctx.Done():
			signal.Stop(ch)
		}
	}()

	return ctx, cancel
}
