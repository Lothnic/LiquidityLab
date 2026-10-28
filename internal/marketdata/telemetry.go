package marketdata

import (
	"context"
	"time"
)

type receiptStartKey struct{}

// SourceReceiptStart returns the local monotonic read-completion origin supplied
// by instrumented Capture callbacks. It is absent for offline/imported events and
// synthetic sources, and is never encoded in a receipt or sent across processes.
func SourceReceiptStart(ctx context.Context) (time.Time, bool) {
	started, ok := ctx.Value(receiptStartKey{}).(time.Time)
	return started, ok && !started.IsZero()
}
