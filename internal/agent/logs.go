package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/makerusa/ivault/internal/config"
)

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Component string `json:"component"`
	Message   string `json:"message"`
}

// Log levels, ranked. Only entries at or above the ship level are sent to the
// portal; everything is still written to stderr (journalctl) locally.
const (
	levelDebug = 0
	levelInfo  = 1
	levelWarn  = 2
	levelError = 3
)

func levelRank(level string) int {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return levelDebug
	case "WARN", "WARNING":
		return levelWarn
	case "ERROR", "FATAL", "PANIC":
		return levelError
	default:
		return levelInfo
	}
}

func rankName(rank int) string {
	switch rank {
	case levelDebug:
		return "DEBUG"
	case levelWarn:
		return "WARN"
	case levelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// classifyLevel infers a level from a plain log message. The codebase logs via
// log.Printf without explicit levels, so we key off content: anything that
// reads like a failure is ERROR, warnings are WARN, the rest is routine INFO.
// (A future refactor to a leveled logger can replace this heuristic.)
func classifyLevel(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "error") || strings.Contains(m, "failed") ||
		strings.Contains(m, "fatal") || strings.Contains(m, "panic") ||
		strings.Contains(m, "could not") || strings.Contains(m, "unable to"):
		return "ERROR"
	case strings.Contains(m, "warn") || strings.Contains(m, "retry") ||
		strings.Contains(m, "skipping") || strings.Contains(m, "stale"):
		return "WARN"
	default:
		return "INFO"
	}
}

type LogCollector struct {
	mu     sync.Mutex
	buffer []LogEntry
	cfg    *config.Config
	// shipLevel is the minimum rank to SEND to the portal (layer 1 filtering).
	// Defaults to INFO; the portal pushes the tier-effective level via heartbeat.
	shipLevel atomic.Int32
}

var collector *LogCollector

// SetLogShipLevel sets the minimum level shipped to the portal. Called from the
// heartbeat response handler when the portal pushes the account/tier-effective
// level. Unknown/empty values are ignored (level stays as-is).
func SetLogShipLevel(level string) {
	if collector == nil || level == "" {
		return
	}
	newRank := int32(levelRank(level))
	if old := collector.shipLevel.Swap(newRank); old != newRank {
		log.Printf("logs: ship level set to %s (was %s)", rankName(int(newRank)), rankName(int(old)))
	}
}

// InitLogs starts the log collection and periodic push.
func InitLogs(ctx context.Context, cfg *config.Config) {
	if cfg.DeviceID == "" || cfg.DeviceAPIKey == "" || cfg.CloudEndpoint == "" {
		return
	}

	collector = &LogCollector{
		cfg:    cfg,
		buffer: make([]LogEntry, 0),
	}
	collector.shipLevel.Store(int32(levelInfo)) // default until the portal pushes a level

	// Set as default logger output
	log.SetOutput(collector)

	go collector.loop(ctx)
}

func (c *LogCollector) Write(p []byte) (n int, err error) {
	msg := string(bytes.TrimSpace(p))

	// Always keep full local logs (journalctl) regardless of ship level.
	fmt.Fprintln(os.Stderr, msg)

	// Layer 1: only buffer for the portal what meets the ship level. This is the
	// cooperative traffic-saving step; the portal independently enforces the
	// tier floor on ingest, so a modified device can't exceed its tier anyway.
	level := classifyLevel(msg)
	if levelRank(level) < int(c.shipLevel.Load()) {
		return len(p), nil
	}

	c.mu.Lock()
	c.buffer = append(c.buffer, LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     level,
		Component: "appliance",
		Message:   msg,
	})
	// Keep buffer reasonable
	if len(c.buffer) > 1000 {
		c.buffer = c.buffer[len(c.buffer)-1000:]
	}
	c.mu.Unlock()

	return len(p), nil
}

func (c *LogCollector) loop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.push()
		}
	}
}

func (c *LogCollector) push() {
	c.mu.Lock()
	if len(c.buffer) == 0 {
		c.mu.Unlock()
		return
	}
	logs := c.buffer
	c.buffer = make([]LogEntry, 0)
	c.mu.Unlock()

	body, _ := json.Marshal(logs)
	url := fmt.Sprintf("%s/api/devices/%s/logs", c.cfg.CloudEndpoint, c.cfg.DeviceID)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent log push req creation error: %v\n", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", c.cfg.UserID)
	req.Header.Set("X-Device-Key", c.cfg.DeviceAPIKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent log push network error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "agent log push failed with status: %d\n", resp.StatusCode)
	}
}
