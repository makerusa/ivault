package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/makerusa/ivault/internal/config"
)

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Component string `json:"component"`
	Message   string `json:"message"`
}

type LogCollector struct {
	mu     sync.Mutex
	buffer []LogEntry
	cfg    *config.Config
}

var collector *LogCollector

// InitLogs starts the log collection and periodic push.
func InitLogs(ctx context.Context, cfg *config.Config) {
	if cfg.DeviceID == "" || cfg.DeviceAPIKey == "" || cfg.CloudEndpoint == "" {
		return
	}

	collector = &LogCollector{
		cfg:    cfg,
		buffer: make([]LogEntry, 0),
	}

	// Set as default logger output
	log.SetOutput(collector)

	go collector.loop(ctx)
}

func (c *LogCollector) Write(p []byte) (n int, err error) {
	msg := string(bytes.TrimSpace(p))
	
	c.mu.Lock()
	c.buffer = append(c.buffer, LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     "INFO", // Default level for standard log
		Component: "appliance",
		Message:   msg,
	})
	// Keep buffer reasonable
	if len(c.buffer) > 1000 {
		c.buffer = c.buffer[len(c.buffer)-1000:]
	}
	c.mu.Unlock()
	
	// Still print to stderr for local debugging
	fmt.Fprintln(os.Stderr, msg)
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
