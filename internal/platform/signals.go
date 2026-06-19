package platform

import (
	"context"
	"os/signal"
)

// ShutdownContext returns a child context cancelled on OS shutdown signals
// (Interrupt on every platform; SIGTERM additionally on Unix). The returned
// stop func releases the signal handler.
func ShutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, shutdownSignals()...)
}
