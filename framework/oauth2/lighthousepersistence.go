package oauth2

import (
	"context"
	"time"
)

// retryOAuthPersistence retries only SQLite lock contention after a successful
// token exchange. It never repeats the exchange and retains the store's CAS.
// Cancellation, permanent errors and a lost CAS stop immediately.
func retryOAuthPersistence(ctx context.Context, write func() (bool, error)) (bool, error) {
	delays := []time.Duration{20 * time.Millisecond, 60 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		updated, err := write()
		if err == nil {
			return updated, nil
		}
		if attempt >= len(delays) || !isLighthouseSQLiteLock(err) {
			return false, err
		}
		timer := time.NewTimer(delays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}
