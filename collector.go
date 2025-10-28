package main

import (
	"context"
	"errors"
	"log"
	"sync"
)



// ErrNoEmitFunc is returned when the caller does not provide an emit callback.
var ErrNoEmitFunc = errors.New("collector: emit func is nil")

// Stream emits metric envelopes from a specific backend (helper, ps, agents, ...).
type Stream interface {
	Start(ctx context.Context) (<-chan Envelope, error)
}

// EmitFunc consumes envelopes produced by Stream implementations.
type EmitFunc func(Envelope) error

// Manager fans in multiple Streams and feeds the central aggregator.
type Manager struct {
	streams []Stream
}

const managerLogPrefix = "[collector/manager] "

func managerLogf(format string, args ...any) {
	log.Printf(managerLogPrefix+format, args...)
}

// NewManager creates a Manager with the supplied streams.
func NewManager(streams ...Stream) *Manager {
	return &Manager{streams: streams}
}

// Run activates all registered streams and forwards samples to emit.
func (m *Manager) Run(ctx context.Context, emit EmitFunc) error {
	if emit == nil {
		return ErrNoEmitFunc
	}
	if len(m.streams) == 0 {
		managerLogf("no streams configured; skipping run")
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(m.streams))

	managerLogf("starting with %d stream(s)", len(m.streams))
	for _, stream := range m.streams {
		stream := stream
		ch, err := stream.Start(ctx)
		if err != nil {
			managerLogf("stream %T failed to start: %v", stream, err)
			cancel()
			return err
		}

		wg.Add(1)
		go func(ch <-chan Envelope) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					managerLogf("context canceled; stopping stream %T", stream)
					return
				case env, ok := <-ch:
					if !ok {
						managerLogf("stream %T channel closed", stream)
						return
					}
					if err := emit(env); err != nil && !errors.Is(err, context.Canceled) {
						select {
						case errCh <- err:
						default:
						}
						managerLogf("emit returned error from stream %T: %v", stream, err)
						cancel()
						return
					}
				}
			}
		}(ch)
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	for {
		select {
		case <-ctx.Done():
			// Drain any emitted errors before returning.
			select {
			case err := <-errCh:
				if err != nil && !errors.Is(err, context.Canceled) {
					managerLogf("returning ctx error with pending stream error: %v", err)
					return err
				}
			default:
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				managerLogf("run canceled by context")
				return nil
			}
			managerLogf("context error: %v", ctx.Err())
			return ctx.Err()
		case err := <-errCh:
			if err != nil {
				managerLogf("stream emitted error: %v", err)
				return err
			}
		case <-doneCh:
			managerLogf("all streams completed")
			return nil
		}
	}
}
