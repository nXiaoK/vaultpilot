package platform

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type networkCounter struct{ Received, Sent uint64 }
type networkSample struct {
	At       time.Time `json:"at"`
	Upload   float64   `json:"upload_bytes_per_second"`
	Download float64   `json:"download_bytes_per_second"`
}
type networkMonitor struct {
	mu         sync.Mutex
	read       func() (map[string]networkCounter, error)
	previous   map[string]networkCounter
	previousAt time.Time
	history    []networkSample
	available  bool
	supported  bool
	closeOnce  sync.Once
	stop, done chan struct{}
}

func parseNetworkCounters(reader io.Reader) (map[string]networkCounter, error) {
	result := map[string]networkCounter{}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.LastIndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 16 {
			return nil, errors.New("invalid network counters")
		}
		rx, e := strconv.ParseUint(fields[0], 10, 64)
		if e != nil {
			return nil, e
		}
		tx, e := strconv.ParseUint(fields[8], 10, 64)
		if e != nil {
			return nil, e
		}
		result[name] = networkCounter{rx, tx}
	}
	if e := scanner.Err(); e != nil {
		return nil, e
	}
	if len(result) == 0 {
		return nil, errors.New("no network interfaces")
	}
	return result, nil
}
func readNetworkCounters() (map[string]networkCounter, error) {
	f, e := os.Open("/proc/net/dev")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return parseNetworkCounters(f)
}
func newNetworkMonitor(read func() (map[string]networkCounter, error)) *networkMonitor {
	m := &networkMonitor{read: read, history: []networkSample{}, stop: make(chan struct{}), done: make(chan struct{})}
	m.sample(time.Now())
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				m.sample(time.Now())
			}
		}
	}()
	return m
}
func (m *networkMonitor) sample(at time.Time) {
	counters, e := m.read()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.available = false
	m.supported = e == nil
	if e != nil {
		m.previous = nil
		m.previousAt = time.Time{}
		return
	}
	seconds := at.Sub(m.previousAt).Seconds()
	valid := len(m.previous) == len(counters) && len(counters) > 0 && seconds > 0 && seconds < 5
	var received, sent uint64
	for name, c := range counters {
		previous, exists := m.previous[name]
		if !exists || c.Received < previous.Received || c.Sent < previous.Sent {
			valid = false
			continue
		}
		received += c.Received - previous.Received
		sent += c.Sent - previous.Sent
	}
	m.previous, m.previousAt = counters, at
	if !valid {
		return
	}
	m.available = true
	m.history = append(m.history, networkSample{at, float64(sent) / seconds, float64(received) / seconds})
	for len(m.history) > 60 || len(m.history) > 0 && m.history[0].At.Before(at.Add(-time.Minute)) {
		m.history = m.history[1:]
	}
}
func (r *Runtime) networkMetrics(w http.ResponseWriter, req *http.Request) {
	m := r.network
	m.mu.Lock()
	at := time.Now()
	history := []networkSample{}
	for _, sample := range m.history {
		if !sample.At.Before(at.Add(-time.Minute)) {
			history = append(history, sample)
		}
	}
	available, supported := m.available && at.Sub(m.previousAt) < 3*time.Second, m.supported
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"available": available, "supported": supported, "history": history, "sample_interval_seconds": 1, "window_seconds": 60, "scope": "node_network", "server_time": at})
}
