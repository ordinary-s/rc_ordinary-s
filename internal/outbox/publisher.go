package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/ordinary-s/rc_ordinary-s/internal/mq"
	"github.com/ordinary-s/rc_ordinary-s/internal/store"
)

type Store interface {
	PendingOutbox(context.Context, int) ([]store.OutboxEvent, error)
	MarkOutboxPublished(context.Context, string) error
	DeferOutbox(context.Context, string, string, time.Duration) error
}

type Sender interface {
	Publish(context.Context, mq.Event) error
}

type Publisher struct {
	store  Store
	sender Sender
	logger *slog.Logger
	poll   time.Duration
}

func New(s Store, sender Sender, logger *slog.Logger, poll time.Duration) *Publisher {
	return &Publisher{store: s, sender: sender, logger: logger, poll: poll}
}

func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		p.publishBatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Publisher) publishBatch(ctx context.Context) {
	events, err := p.store.PendingOutbox(ctx, 100)
	if err != nil {
		if ctx.Err() == nil {
			p.logger.Error("outbox scan failed", "code", "OUTBOX_SCAN_FAILED", "error_type", fmt.Sprintf("%T", err))
		}
		return
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return
		}
		msg := mq.Event{EventID: event.EventID, TaskID: event.TaskID, TraceID: event.TraceID}
		sendCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
		err := p.sender.Publish(sendCtx, msg)
		cancel()
		if err != nil {
			delay := retryDelay(event.PublishAttempts)
			summary := fmt.Sprintf("MQ_PUBLISH_UNCONFIRMED:%T", err)
			if deferErr := p.store.DeferOutbox(ctx, event.EventID, summary, delay); deferErr != nil && ctx.Err() == nil {
				p.logger.Error("outbox retry schedule failed", "event_id", event.EventID, "task_id", event.TaskID, "code", "OUTBOX_DEFER_FAILED", "error_type", fmt.Sprintf("%T", deferErr))
			}
			p.logger.Warn("outbox send not confirmed", "event_id", event.EventID, "task_id", event.TaskID, "trace_id", event.TraceID, "retry_in_ms", delay.Milliseconds())
			continue
		}
		if err := p.store.MarkOutboxPublished(ctx, event.EventID); err != nil && ctx.Err() == nil {
			p.logger.Error("outbox acknowledgement update failed", "event_id", event.EventID, "task_id", event.TaskID, "trace_id", event.TraceID, "code", "OUTBOX_MARK_FAILED", "error_type", fmt.Sprintf("%T", err))
		}
	}
}

func retryDelay(attempts int) time.Duration {
	seconds := math.Pow(2, float64(attempts))
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}
