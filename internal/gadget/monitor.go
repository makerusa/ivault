package gadget

import (
	"context"
	"log"
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
	udcName  string
	// debounceSamples is how many consecutive polls a new state must persist
	// for before it is treated as a real change. This filters out brief USB
	// link resets / re-enumeration (common on flaky cables or SuperSpeed PHYs)
	// that would otherwise fire spurious plug/unplug events and trigger false
	// maintenance cycles.
	debounceSamples int
}

func NewMonitor(udcName string) *Monitor {
	return &Monitor{
		events:          make(chan UDCEvent, 4),
		interval:        500 * time.Millisecond,
		udcName:         udcName,
		debounceSamples: 3, // ~1.5s of stability required before acting
	}
}

func (m *Monitor) Events() <-chan UDCEvent {
	return m.events
}

func (m *Monitor) Start(ctx context.Context) {
	go func() {
		// Initialize last state without emitting an event
		m.last = State(m.udcName)
		candidate := m.last // the state we are currently seeing settle
		stable := 0         // consecutive polls candidate has held

		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.interval):
				current := State(m.udcName)

				// No pending change from the committed state — clear any
				// in-flight candidate (e.g. a transient flap that reverted).
				if current == m.last {
					candidate = current
					stable = 0
					continue
				}

				// A change is pending. Require it to persist for
				// debounceSamples consecutive polls before acting so brief
				// link resets do not register as plug/unplug events.
				if current == candidate {
					stable++
				} else {
					candidate = current
					stable = 1
					log.Printf("gadget: UDC state changing: %s -> %s (settling)", m.last, current)
				}

				if stable < m.debounceSamples {
					continue
				}

				prev := m.last
				m.last = current
				candidate = current
				stable = 0
				log.Printf("gadget: UDC state changed: %s -> %s", prev, current)

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
	return isNotAttached(prev) && !isNotAttached(current)
}

func isUnplugged(prev, current string) bool {
	return !isNotAttached(prev) && isNotAttached(current)
}

func isNotAttached(state string) bool {
	s := strings.TrimSpace(state)
	return s == "not attached" || s == "unknown"
}
