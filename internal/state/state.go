package state

import (
	"sync"
)

type State int

const (
	StateProvisioning State = iota
	StateBooting
	StateRecording
	StateDetaching
	StateMaintenance
	StateAttaching
	StateUploading
	StateOffline
	StateError
	StateShuttingDown
)

func (s State) String() string {
	switch s {
	case StateProvisioning:
		return "provisioning"
	case StateBooting:
		return "booting"
	case StateRecording:
		return "recording"
	case StateDetaching:
		return "detaching"
	case StateMaintenance:
		return "maintenance"
	case StateAttaching:
		return "attaching"
	case StateUploading:
		return "uploading"
	case StateOffline:
		return "offline"
	case StateError:
		return "error"
	case StateShuttingDown:
		return "shutting_down"
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
