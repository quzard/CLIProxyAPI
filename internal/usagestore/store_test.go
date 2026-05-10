package usagestore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRecordsSnapshotAndReloadsJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFilename)
	store := NewStore()
	if err := store.Configure(path); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	payload := []byte(`{
		"provider":"openai",
		"model":"gpt-5.4",
		"alias":"client-gpt",
		"endpoint":"POST /v1/chat/completions",
		"auth_type":"apikey",
		"api_key":"secret-user-key",
		"request_id":"req-1",
		"timestamp":"2026-05-10T00:00:00Z",
		"latency_ms":123,
		"source":"user@example.com",
		"auth_index":"0",
		"tokens":{"input_tokens":10,"output_tokens":20,"reasoning_tokens":3,"cached_tokens":2},
		"failed":false
	}`)
	if err := store.RecordPayload(payload); err != nil {
		t.Fatalf("RecordPayload() error = %v", err)
	}
	if err := store.RecordPayload(payload); err != nil {
		t.Fatalf("duplicate RecordPayload() error = %v", err)
	}

	snapshot := store.Snapshot()
	if snapshot.TotalRequests != 1 || snapshot.SuccessCount != 1 || snapshot.FailureCount != 0 {
		t.Fatalf("snapshot counts = total:%d success:%d failure:%d, want 1/1/0", snapshot.TotalRequests, snapshot.SuccessCount, snapshot.FailureCount)
	}
	if snapshot.TotalTokens != 33 {
		t.Fatalf("snapshot total tokens = %d, want 33", snapshot.TotalTokens)
	}
	apiStats := snapshot.APIs["POST /v1/chat/completions"]
	if apiStats == nil {
		t.Fatal("missing endpoint stats")
	}
	modelStats := apiStats.Models["gpt-5.4"]
	if modelStats == nil || modelStats.TotalRequests != 1 || len(modelStats.Details) != 1 {
		t.Fatalf("model stats = %#v, want one detail", modelStats)
	}
	if modelStats.Details[0].RequestID != "req-1" {
		t.Fatalf("detail request_id = %q, want req-1", modelStats.Details[0].RequestID)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), "secret-user-key") {
		t.Fatal("persisted usage event leaked api_key")
	}

	reloaded := NewStore()
	if err := reloaded.Configure(path); err != nil {
		t.Fatalf("reload Configure() error = %v", err)
	}
	reloadedSnapshot := reloaded.Snapshot()
	if reloadedSnapshot.TotalRequests != 1 || reloadedSnapshot.TotalTokens != 33 {
		t.Fatalf("reloaded snapshot = total:%d tokens:%d, want 1/33", reloadedSnapshot.TotalRequests, reloadedSnapshot.TotalTokens)
	}
}

func TestStoreImportsExportedSnapshotWithDeduplication(t *testing.T) {
	source := NewStore()
	if err := source.Configure(filepath.Join(t.TempDir(), DefaultFilename)); err != nil {
		t.Fatalf("source Configure() error = %v", err)
	}
	if err := source.RecordPayload([]byte(`{
		"model":"claude-sonnet-4",
		"endpoint":"POST /v1/messages",
		"request_id":"req-import",
		"timestamp":"2026-05-10T01:00:00Z",
		"tokens":{"input_tokens":7,"output_tokens":11,"total_tokens":18},
		"failed":true,
		"fail":{"status_code":429,"body":"rate limited"}
	}`)); err != nil {
		t.Fatalf("RecordPayload() error = %v", err)
	}

	exported, err := json.Marshal(source.Export())
	if err != nil {
		t.Fatalf("Marshal export error = %v", err)
	}

	target := NewStore()
	if err := target.Configure(filepath.Join(t.TempDir(), DefaultFilename)); err != nil {
		t.Fatalf("target Configure() error = %v", err)
	}
	result, err := target.Import(exported)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if result.Added != 1 || result.Skipped != 0 || result.TotalRequests != 1 || result.FailedRequests != 1 {
		t.Fatalf("import result = %#v, want added=1 skipped=0 total=1 failed=1", result)
	}

	result, err = target.Import(exported)
	if err != nil {
		t.Fatalf("duplicate Import() error = %v", err)
	}
	if result.Added != 0 || result.Skipped != 1 || result.TotalRequests != 1 || result.FailedRequests != 1 {
		t.Fatalf("duplicate import result = %#v, want added=0 skipped=1 total=1 failed=1", result)
	}
}
