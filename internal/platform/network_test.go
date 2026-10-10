package platform

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNetworkRatesAndDiscontinuities(t *testing.T) {
	counters := map[string]networkCounter{"eth0": {100, 200}}
	var readErr error
	m := &networkMonitor{read: func() (map[string]networkCounter, error) {
		copy := map[string]networkCounter{}
		for k, v := range counters {
			copy[k] = v
		}
		return copy, readErr
	}}
	at := time.Now()
	m.sample(at)
	if m.available || len(m.history) != 0 {
		t.Fatal("first counter must only establish the baseline")
	}
	counters["eth0"] = networkCounter{2100, 6200}
	m.sample(at.Add(2 * time.Second))
	if !m.available || len(m.history) != 1 || m.history[0].Upload != 3000 || m.history[0].Download != 1000 {
		t.Fatalf("incorrect byte rates: %+v", m.history)
	}
	counters["eth0"] = networkCounter{1, 1}
	m.sample(at.Add(3 * time.Second))
	if m.available || len(m.history) != 1 {
		t.Fatal("counter reset must not produce a bogus speed")
	}
	readErr = errors.New("unavailable")
	m.sample(at.Add(4 * time.Second))
	readErr = nil
	m.sample(at.Add(5 * time.Second))
	if m.available {
		t.Fatal("recovery must establish a new baseline")
	}
	for i := 6; i < 100; i++ {
		counters["eth0"] = networkCounter{uint64(i * 100), uint64(i * 200)}
		m.sample(at.Add(time.Duration(i) * time.Second))
	}
	if len(m.history) != 60 || !m.available {
		t.Fatalf("history must stay bounded: %d", len(m.history))
	}
	counters["eth1"] = networkCounter{99999, 99999}
	m.sample(at.Add(100 * time.Second))
	if m.available {
		t.Fatal("new interfaces must not introduce an artificial spike")
	}
}
func TestParseNetworkCounters(t *testing.T) {
	input := "Inter-| Receive | Transmit\n lo: 99999 0 0 0 0 0 0 0 99999 0 0 0 0 0 0 0\n eth0: 123 0 0 0 0 0 0 0 456 0 0 0 0 0 0 0\n"
	counters, e := parseNetworkCounters(strings.NewReader(input))
	if e != nil || len(counters) != 1 || counters["eth0"] != (networkCounter{123, 456}) {
		t.Fatalf("parse: %+v %v", counters, e)
	}
	if _, e := parseNetworkCounters(strings.NewReader("eth0: invalid\n")); e == nil {
		t.Fatal("invalid counters accepted")
	}
}
func TestNetworkMetricsRequiresAuthentication(t *testing.T) {
	h := newHarness(t, false)
	res := h.call("GET", "/api/metrics/network", nil, false)
	var result struct {
		Scope   string          `json:"scope"`
		History []networkSample `json:"history"`
	}
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &result) != nil || result.Scope != "node_network" || result.History == nil {
		t.Fatalf("metrics response: %s", res.Body.String())
	}
	h.cookie = nil
	if res = h.call("GET", "/api/metrics/network", nil, false); res.Code != 401 {
		t.Fatal("network metrics exposed without authentication")
	}
}
