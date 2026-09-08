package clickup

import (
	"context"
	"time"
)

// limiter — простой token-bucket без внешних зависимостей: ограничивает
// количество исходящих запросов к ClickUp API (~100/мин по документации).
type limiter struct {
	tokens chan struct{}
	stop   chan struct{}
}

func newLimiter(perMinute int) *limiter {
	if perMinute <= 0 {
		perMinute = 100
	}
	l := &limiter{
		tokens: make(chan struct{}, perMinute),
		stop:   make(chan struct{}),
	}
	for i := 0; i < perMinute; i++ {
		l.tokens <- struct{}{}
	}

	interval := time.Minute / time.Duration(perMinute)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case l.tokens <- struct{}{}:
				default:
					// бакет полон, лишний тик пропускаем
				}
			case <-l.stop:
				return
			}
		}
	}()
	return l
}

func (l *limiter) Wait(ctx context.Context) error {
	select {
	case <-l.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *limiter) Close() {
	close(l.stop)
}
