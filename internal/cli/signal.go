package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// signalContext returns a context cancelled on SIGINT or SIGTERM, so an
// in-flight DigiKey request is abandoned promptly on Ctrl-C.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
