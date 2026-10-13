package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type requestRecord struct {
	Method         string `json:"method"`
	Path           string `json:"path"`
	ContentType    string `json:"content_type,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	TraceID        string `json:"trace_id,omitempty"`
	Body           string `json:"body"`
}

type target struct {
	mu           sync.Mutex
	statuses     []int
	delay        time.Duration
	responseBody string
	count        int
	inFlight     int
	maxInFlight  int
	history      []requestRecord
}

func main() {
	state := &target{statuses: []int{200}, responseBody: "ok", history: make([]requestRecord, 0, 100)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /control/configure", state.configure)
	mux.HandleFunc("POST /control/reset", state.reset)
	mux.HandleFunc("GET /control/stats", state.stats)
	mux.HandleFunc("/notify", state.notify)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	server := &http.Server{Addr: ":8090", Handler: mux, ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func (t *target) configure(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Statuses     []int  `json:"statuses"`
		DelayMS      int    `json:"delay_ms"`
		ResponseBody string `json:"response_body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&input); err != nil {
		http.Error(w, "invalid configuration", http.StatusBadRequest)
		return
	}
	if len(input.Statuses) == 0 {
		input.Statuses = []int{200}
	}
	if len(input.Statuses) > 20 || input.DelayMS < 0 || input.DelayMS > 10000 {
		http.Error(w, "configuration is out of bounds", http.StatusBadRequest)
		return
	}
	for _, status := range input.Statuses {
		if status < 200 || status > 599 {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
	}
	t.mu.Lock()
	t.statuses = append([]int(nil), input.Statuses...)
	t.delay = time.Duration(input.DelayMS) * time.Millisecond
	t.responseBody = input.ResponseBody
	t.count = 0
	t.inFlight = 0
	t.maxInFlight = 0
	t.history = t.history[:0]
	t.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (t *target) reset(w http.ResponseWriter, _ *http.Request) {
	t.mu.Lock()
	t.statuses = []int{200}
	t.delay = 0
	t.responseBody = "ok"
	t.count = 0
	t.inFlight = 0
	t.maxInFlight = 0
	t.history = t.history[:0]
	t.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (t *target) stats(w http.ResponseWriter, _ *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	write := struct {
		Count       int             `json:"count"`
		InFlight    int             `json:"in_flight"`
		MaxInFlight int             `json:"max_in_flight"`
		Requests    []requestRecord `json:"requests"`
	}{Count: t.count, InFlight: t.inFlight, MaxInFlight: t.maxInFlight, Requests: append([]requestRecord(nil), t.history...)}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(write)
}

func (t *target) notify(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 512*1024))
	_ = r.Body.Close()
	record := requestRecord{
		Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"),
		IdempotencyKey: r.Header.Get("Idempotency-Key"), TaskID: r.Header.Get("X-Notification-Task-ID"),
		TraceID: r.Header.Get("X-Trace-ID"), Body: string(body),
	}
	if len(record.Body) > 2048 {
		record.Body = record.Body[:2048]
	}
	t.mu.Lock()
	t.count++
	position := t.count - 1
	status := t.statuses[len(t.statuses)-1]
	if position < len(t.statuses) {
		status = t.statuses[position]
	}
	delay := t.delay
	responseBody := t.responseBody
	t.inFlight++
	if t.inFlight > t.maxInFlight {
		t.maxInFlight = t.inFlight
	}
	t.history = append(t.history, record)
	if len(t.history) > 1000 {
		t.history = append([]requestRecord(nil), t.history[len(t.history)-1000:]...)
	}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.inFlight--
		t.mu.Unlock()
	}()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, strings.TrimSpace(responseBody))
}
