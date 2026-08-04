package commands

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/LensBridge/agent/internal/wsclient"
)

const (
	defaultDeadline = 30 * time.Second
	cacheCapacity   = 256
)

// Dispatcher implements wsclient.CommandHandler. It looks up a Handler in the
// Registry, runs it under the deadline, dedupes by commandId, and emits
// ack/progress/result frames via the supplied wsclient.Sender.
type Dispatcher struct {
	registry *Registry
	cache    *ResultCache
	logger   *slog.Logger
}

func NewDispatcher(registry *Registry, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		registry: registry,
		cache:    NewResultCache(cacheCapacity),
		logger:   logger,
	}
}

// Handle is the wsclient.CommandHandler entry point. Spawned in its own
// goroutine by wsclient — long-running handlers will not block heartbeats.
func (d *Dispatcher) Handle(ctx context.Context, sender wsclient.Sender, cmd wsclient.Command) {
	// Replay path: identical commandId from a backend reconnect/queue flush.
	if prior, ok := d.cache.Get(cmd.ID); ok {
		d.logger.Info("re-emitting cached result", "commandId", cmd.ID, "kind", cmd.Kind, "status", prior.Status)
		_ = sender.Ack(ctx, cmd.ID)
		_ = sender.Result(ctx, cmd.ID, prior.Status, prior.Output, prior.ErrorMessage, prior.DurationMs)
		return
	}

	handler, ok := d.registry.Lookup(cmd.Kind)
	if !ok {
		d.logger.Warn("rejecting unknown command kind", "kind", cmd.Kind, "commandId", cmd.ID)
		_ = sender.Ack(ctx, cmd.ID)
		result := CachedResult{Status: "rejected", ErrorMessage: "unknown_kind: " + cmd.Kind}
		d.cache.Put(cmd.ID, result)
		_ = sender.Result(ctx, cmd.ID, result.Status, nil, result.ErrorMessage, 0)
		return
	}

	if err := sender.Ack(ctx, cmd.ID); err != nil {
		d.logger.Warn("failed to send ack", "commandId", cmd.ID, "err", err)
		return
	}

	deadline := defaultDeadline
	if cmd.DeadlineMs > 0 {
		deadline = time.Duration(cmd.DeadlineMs) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	progress := func(stage, message string, percent *int) {
		_ = sender.Progress(runCtx, cmd.ID, stage, message, percent)
	}

	start := time.Now()
	output, err := handler.Execute(runCtx, cmd.Payload, progress)
	durationMs := time.Since(start).Milliseconds()

	result := CachedResult{Status: "ok", Output: output, DurationMs: durationMs}
	switch {
	case err == nil:
		// keep status ok
	case errors.Is(err, ErrRejected):
		result.Status = "rejected"
		result.ErrorMessage = err.Error()
		result.Output = nil
	case errors.Is(err, context.DeadlineExceeded):
		result.Status = "timeout"
		result.ErrorMessage = err.Error()
		result.Output = nil
	default:
		result.Status = "error"
		result.ErrorMessage = err.Error()
		result.Output = nil
	}

	d.cache.Put(cmd.ID, result)
	if sendErr := sender.Result(ctx, cmd.ID, result.Status, result.Output, result.ErrorMessage, result.DurationMs); sendErr != nil {
		d.logger.Warn("failed to send result", "commandId", cmd.ID, "err", sendErr)
	}
	d.logger.Info("command finished",
		"commandId", cmd.ID,
		"kind", cmd.Kind,
		"status", result.Status,
		"durationMs", durationMs,
	)
}
