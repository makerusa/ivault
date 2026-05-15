package state

import (
	"sync"
)

type State int

const (
	StateBooting State = iota
	StateDisconnected
	StateConnecting
	StateConnected
	StateDisconnecting
	StateSyncing
	StateError
)

func (s State) String() string {
	switch s {
	case StateBooting:
		return "booting"
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateDisconnecting:
		return "disconnecting"
	case StateSyncing:
		return "syncing"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

type Machine struct {
	mu       sync.RWMutex
	current  State
	onChange []func(old, new State)
}

func New() *Machine {
	return &Machine{current: StateBooting}
}

func (m *Machine) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *Machine) OnChange(fn func(old, new State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

func (m *Machine) Transition(next State) {
	m.mu.Lock()
	// Guard against same-state transitions
	if m.current == next {
		m.mu.Unlock()
		return
	}
	old := m.current
	m.current = next
	handlers := m.onChange
	m.mu.Unlock()

	for _, fn := range handlers {
		fn(old, next)
	}
}
