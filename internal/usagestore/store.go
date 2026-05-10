package usagestore

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const DefaultFilename = "usage.jsonl"

type ExportPayload struct {
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exported_at"`
	Usage      Snapshot  `json:"usage"`
}

type ImportResult struct {
	Added          int   `json:"added"`
	Skipped        int   `json:"skipped"`
	TotalRequests  int64 `json:"total_requests"`
	FailedRequests int64 `json:"failed_requests"`
}

type Snapshot struct {
	TotalRequests int64                `json:"total_requests"`
	SuccessCount  int64                `json:"success_count"`
	FailureCount  int64                `json:"failure_count"`
	TotalTokens   int64                `json:"total_tokens"`
	APIs          map[string]*APIStats `json:"apis"`
}

type APIStats struct {
	TotalRequests int64                  `json:"total_requests"`
	SuccessCount  int64                  `json:"success_count"`
	FailureCount  int64                  `json:"failure_count"`
	TotalTokens   int64                  `json:"total_tokens"`
	Models        map[string]*ModelStats `json:"models"`
}

type ModelStats struct {
	TotalRequests int64           `json:"total_requests"`
	SuccessCount  int64           `json:"success_count"`
	FailureCount  int64           `json:"failure_count"`
	TotalTokens   int64           `json:"total_tokens"`
	Details       []RequestDetail `json:"details"`
}

type Event struct {
	RequestDetail
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Alias    string `json:"alias,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	AuthType string `json:"auth_type,omitempty"`
}

type RequestDetail struct {
	Timestamp time.Time  `json:"timestamp"`
	LatencyMs int64      `json:"latency_ms,omitempty"`
	Source    string     `json:"source,omitempty"`
	AuthIndex string     `json:"auth_index,omitempty"`
	Tokens    TokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
	Fail      FailDetail `json:"fail,omitempty"`
	RequestID string     `json:"request_id,omitempty"`
}

type TokenStats struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

type FailDetail struct {
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	loaded   bool
	snapshot Snapshot
	seen     map[string]struct{}
}

var defaultStore = NewStore()

func NewStore() *Store {
	return &Store{
		snapshot: emptySnapshot(),
		seen:     make(map[string]struct{}),
	}
}

func ConfigureForAuthDir(authDir string) error {
	return defaultStore.ConfigureForAuthDir(authDir)
}

func RecordPayload(payload []byte) {
	if err := defaultStore.RecordPayload(payload); err != nil {
		log.WithError(err).Warn("usage store: failed to persist usage payload")
	}
}

func GetSnapshot() Snapshot {
	return defaultStore.Snapshot()
}

func Export() ExportPayload {
	return defaultStore.Export()
}

func Import(payload []byte) (ImportResult, error) {
	return defaultStore.Import(payload)
}

func (s *Store) ConfigureForAuthDir(authDir string) error {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return nil
	}
	path := filepath.Join(authDir, DefaultFilename)
	return s.Configure(path)
}

func (s *Store) Configure(path string) error {
	if s == nil {
		return nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	clean, errAbs := filepath.Abs(path)
	if errAbs == nil {
		path = clean
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded && s.path == path {
		return nil
	}

	next := emptySnapshot()
	seen := make(map[string]struct{})
	if errLoad := loadFile(path, func(event Event) {
		normalizeEvent(&event)
		key := eventKey(event)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		applyEvent(&next, event)
	}); errLoad != nil {
		return errLoad
	}

	s.path = path
	s.loaded = true
	s.snapshot = next
	s.seen = seen
	return nil
}

func (s *Store) RecordPayload(payload []byte) error {
	if s == nil || len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	var event Event
	if errUnmarshal := json.Unmarshal(payload, &event); errUnmarshal != nil {
		return fmt.Errorf("decode usage payload: %w", errUnmarshal)
	}
	return s.RecordEvent(event)
}

func (s *Store) RecordEvent(event Event) error {
	if s == nil {
		return nil
	}
	normalizeEvent(&event)
	key := eventKey(event)
	if key == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded && strings.TrimSpace(s.path) == "" {
		return nil
	}
	if _, ok := s.seen[key]; ok {
		return nil
	}
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if strings.TrimSpace(s.path) != "" {
		if errAppend := appendEvent(s.path, event); errAppend != nil {
			return errAppend
		}
	}
	s.seen[key] = struct{}{}
	applyEvent(&s.snapshot, event)
	return nil
}

func (s *Store) Snapshot() Snapshot {
	if s == nil {
		return emptySnapshot()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSnapshot(s.snapshot)
}

func (s *Store) Export() ExportPayload {
	return ExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      s.Snapshot(),
	}
}

func (s *Store) Import(payload []byte) (ImportResult, error) {
	events, errParse := parseImportEvents(payload)
	if errParse != nil {
		return ImportResult{}, errParse
	}

	result := ImportResult{}
	for _, event := range events {
		normalizeEvent(&event)
		key := eventKey(event)
		if key == "" {
			result.Skipped++
			continue
		}

		added, errRecord := s.recordEventResult(event, key)
		if errRecord != nil {
			return result, errRecord
		}
		if added {
			result.Added++
		} else {
			result.Skipped++
		}
	}

	snapshot := s.Snapshot()
	result.TotalRequests = snapshot.TotalRequests
	result.FailedRequests = snapshot.FailureCount
	return result, nil
}

func (s *Store) recordEventResult(event Event, key string) (bool, error) {
	if s == nil {
		return false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded && strings.TrimSpace(s.path) == "" {
		return false, nil
	}
	if _, ok := s.seen[key]; ok {
		return false, nil
	}
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if strings.TrimSpace(s.path) != "" {
		if errAppend := appendEvent(s.path, event); errAppend != nil {
			return false, errAppend
		}
	}
	s.seen[key] = struct{}{}
	applyEvent(&s.snapshot, event)
	return true, nil
}

func loadFile(path string, emit func(Event)) error {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		if errors.Is(errOpen, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open usage store: %w", errOpen)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("usage store: failed to close usage file")
		}
	}()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event Event
		if errUnmarshal := json.Unmarshal(line, &event); errUnmarshal != nil {
			log.WithError(errUnmarshal).Warn("usage store: skipped invalid usage record")
			continue
		}
		emit(event)
	}
	if errScan := scanner.Err(); errScan != nil {
		return fmt.Errorf("scan usage store: %w", errScan)
	}
	return nil
}

func appendEvent(path string, event Event) error {
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return fmt.Errorf("create usage store directory: %w", errMkdir)
	}
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open usage store for append: %w", errOpen)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("usage store: failed to close usage file")
		}
	}()

	encoded, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		return fmt.Errorf("encode usage event: %w", errMarshal)
	}
	if _, errWrite := file.Write(append(encoded, '\n')); errWrite != nil {
		return fmt.Errorf("append usage event: %w", errWrite)
	}
	return nil
}

func parseImportEvents(payload []byte) ([]Event, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, fmt.Errorf("usage import payload is empty")
	}

	var array []Event
	if payload[0] == '[' {
		if errUnmarshal := json.Unmarshal(payload, &array); errUnmarshal != nil {
			return nil, fmt.Errorf("decode usage event array: %w", errUnmarshal)
		}
		return array, nil
	}

	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(payload, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("decode usage import payload: %w", errUnmarshal)
	}
	if len(bytes.TrimSpace(envelope.Usage)) > 0 {
		payload = envelope.Usage
	}

	var snapshot Snapshot
	if errUnmarshal := json.Unmarshal(payload, &snapshot); errUnmarshal != nil {
		return nil, fmt.Errorf("decode usage snapshot: %w", errUnmarshal)
	}
	return eventsFromSnapshot(snapshot), nil
}

func eventsFromSnapshot(snapshot Snapshot) []Event {
	if len(snapshot.APIs) == 0 {
		return nil
	}
	events := make([]Event, 0, snapshot.TotalRequests)
	for endpoint, apiStats := range snapshot.APIs {
		if apiStats == nil {
			continue
		}
		for model, modelStats := range apiStats.Models {
			if modelStats == nil {
				continue
			}
			for _, detail := range modelStats.Details {
				event := Event{
					RequestDetail: detail,
					Model:         model,
					Alias:         model,
					Endpoint:      endpoint,
				}
				events = append(events, event)
			}
		}
	}
	return events
}

func normalizeEvent(event *Event) {
	if event == nil {
		return
	}
	event.Provider = strings.TrimSpace(event.Provider)
	event.Model = strings.TrimSpace(event.Model)
	event.Alias = strings.TrimSpace(event.Alias)
	event.Endpoint = strings.TrimSpace(event.Endpoint)
	event.AuthType = strings.TrimSpace(event.AuthType)
	event.Source = strings.TrimSpace(event.Source)
	event.AuthIndex = strings.TrimSpace(event.AuthIndex)
	event.RequestDetail.RequestID = strings.TrimSpace(event.RequestDetail.RequestID)
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.Timestamp = event.Timestamp.UTC()
	if event.Endpoint == "" {
		event.Endpoint = "unknown"
	}
	if event.Model == "" {
		event.Model = strings.TrimSpace(event.Alias)
	}
	if event.Model == "" {
		event.Model = "unknown"
	}
	if event.Tokens.TotalTokens == 0 {
		event.Tokens.TotalTokens = event.Tokens.InputTokens + event.Tokens.OutputTokens + event.Tokens.ReasoningTokens
	}
	if event.Tokens.TotalTokens == 0 {
		event.Tokens.TotalTokens = event.Tokens.InputTokens + event.Tokens.OutputTokens + event.Tokens.ReasoningTokens + event.Tokens.CachedTokens
	}
	if !event.Failed && event.Fail.StatusCode >= 400 {
		event.Failed = true
	}
}

func applyEvent(snapshot *Snapshot, event Event) {
	if snapshot.APIs == nil {
		snapshot.APIs = make(map[string]*APIStats)
	}

	detail := event.RequestDetail
	detail.Tokens = event.Tokens

	snapshot.TotalRequests++
	snapshot.TotalTokens += event.Tokens.TotalTokens
	if event.Failed {
		snapshot.FailureCount++
	} else {
		snapshot.SuccessCount++
	}

	apiStats := snapshot.APIs[event.Endpoint]
	if apiStats == nil {
		apiStats = &APIStats{Models: make(map[string]*ModelStats)}
		snapshot.APIs[event.Endpoint] = apiStats
	}
	apiStats.TotalRequests++
	apiStats.TotalTokens += event.Tokens.TotalTokens
	if event.Failed {
		apiStats.FailureCount++
	} else {
		apiStats.SuccessCount++
	}

	modelStats := apiStats.Models[event.Model]
	if modelStats == nil {
		modelStats = &ModelStats{}
		apiStats.Models[event.Model] = modelStats
	}
	modelStats.TotalRequests++
	modelStats.TotalTokens += event.Tokens.TotalTokens
	if event.Failed {
		modelStats.FailureCount++
	} else {
		modelStats.SuccessCount++
	}
	modelStats.Details = append(modelStats.Details, detail)
}

func eventKey(event Event) string {
	keyPayload := struct {
		Timestamp time.Time  `json:"timestamp"`
		RequestID string     `json:"request_id"`
		Endpoint  string     `json:"endpoint"`
		Model     string     `json:"model"`
		Source    string     `json:"source"`
		AuthIndex string     `json:"auth_index"`
		LatencyMs int64      `json:"latency_ms"`
		Tokens    TokenStats `json:"tokens"`
		Failed    bool       `json:"failed"`
	}{
		Timestamp: event.Timestamp,
		RequestID: event.RequestDetail.RequestID,
		Endpoint:  event.Endpoint,
		Model:     event.Model,
		Source:    event.Source,
		AuthIndex: event.AuthIndex,
		LatencyMs: event.LatencyMs,
		Tokens:    event.Tokens,
		Failed:    event.Failed,
	}
	encoded, errMarshal := json.Marshal(keyPayload)
	if errMarshal != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "hash:" + hex.EncodeToString(sum[:])
}

func emptySnapshot() Snapshot {
	return Snapshot{APIs: make(map[string]*APIStats)}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	out := Snapshot{
		TotalRequests: snapshot.TotalRequests,
		SuccessCount:  snapshot.SuccessCount,
		FailureCount:  snapshot.FailureCount,
		TotalTokens:   snapshot.TotalTokens,
		APIs:          make(map[string]*APIStats, len(snapshot.APIs)),
	}
	for endpoint, apiStats := range snapshot.APIs {
		if apiStats == nil {
			continue
		}
		apiCopy := &APIStats{
			TotalRequests: apiStats.TotalRequests,
			SuccessCount:  apiStats.SuccessCount,
			FailureCount:  apiStats.FailureCount,
			TotalTokens:   apiStats.TotalTokens,
			Models:        make(map[string]*ModelStats, len(apiStats.Models)),
		}
		for model, modelStats := range apiStats.Models {
			if modelStats == nil {
				continue
			}
			details := append([]RequestDetail(nil), modelStats.Details...)
			apiCopy.Models[model] = &ModelStats{
				TotalRequests: modelStats.TotalRequests,
				SuccessCount:  modelStats.SuccessCount,
				FailureCount:  modelStats.FailureCount,
				TotalTokens:   modelStats.TotalTokens,
				Details:       details,
			}
		}
		out.APIs[endpoint] = apiCopy
	}
	return out
}
