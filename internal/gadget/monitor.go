package gadget

import (
	"context"
	"strings"
	"time"
)

type UDCEvent int

const (
	UDCPlugged   UDCEvent = iota // not attached → configured
	UDCUnplugged                 // configured → not attached
)

func (e UDCEvent) String() string {
	switch e {
	case UDCPlugged:
		return "plugged"
	case UDCUnplugged:
		return "unplugged"
	default:
		return "unknown"
	}
}

type Monitor struct {
	events   chan UDCEvent
	last     string
	interval time.Duration
}

func NewMonitor() *Monitor {
	return &Monitor{
		events:   make(chan UDCEvent, 4),
		interval: 500 * time.Millisecond,
	}
}

func (m *Monitor) Events() <-chan UDCEvent {
	return m.events
}

func (m *Monitor) Start(ctx context.Context) {
	go func() {
		// Initialize last state without emitting an event
		m.last = State()

		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.interval):
				current := State()

				// Debounce — only act on stable state change
				if current == m.last {
					continue
				}

				prev := m.last
				m.last = current

				if isPlugged(prev, current) {
					select {
					case m.events <- UDCPlugged:
					default:
					}
				} else if isUnplugged(prev, current) {
					select {
					case m.events <- UDCUnplugged:
					default:
					}
				}
			}
		}
	}()
}

func isPlugged(prev, current string) bool {
	return !isConfigured(prev) && isConfigured(current)
}

func isUnplugged(prev, current string) bool {
	return isConfigured(prev) && !isConfigured(current)
}

func isConfigured(state string) bool {
	return strings.TrimSpace(state) == "configured"
}
