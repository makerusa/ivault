package agent

import (
	"encoding/json"
	"sync"
)

var (
	destMu            sync.RWMutex
	activeDestinations []json.RawMessage
)

func UpdateActiveDestinations(dests []json.RawMessage) {
	destMu.Lock()
	defer destMu.Unlock()
	activeDestinations = dests
}

func GetActiveDestinations() []json.RawMessage {
	destMu.RLock()
	defer destMu.RUnlock()
	return activeDestinations
}
