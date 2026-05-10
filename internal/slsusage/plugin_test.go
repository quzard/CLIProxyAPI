package slsusage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestFromConfigDefaultDisabled(t *testing.T) {
	cfg := fromConfig(internalconfig.SLSWebTrackingConfig{})
	plugin := NewPlugin(cfg)
	if plugin.isEnabled() {
		t.Fatal("SLS WebTracking should be disabled by default")
	}
	plugin.mu.RLock()
	got := plugin.cfg
	plugin.mu.RUnlock()
	if got.Topic != defaultTopic {
		t.Fatalf("topic = %q, want %q", got.Topic, defaultTopic)
	}
	if got.Source == "" {
		t.Fatal("source should have a default")
	}
}

func TestBuildTrackURL(t *testing.T) {
	cfg := normalizeConfig(Config{
		Enabled:  true,
		Project:  "test-project",
		Logstore: "usage-log",
		Endpoint: "cn-hangzhou.log.aliyuncs.com",
	})
	got, err := buildTrackURL(cfg)
	if err != nil {
		t.Fatalf("buildTrackURL error: %v", err)
	}
	want := "https://test-project.cn-hangzhou.log.aliyuncs.com/logstores/usage-log/track"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	cfg.Endpoint = "https://test-project.cn-hangzhou.log.aliyuncs.com"
	got, err = buildTrackURL(cfg)
	if err != nil {
		t.Fatalf("buildTrackURL with project host error: %v", err)
	}
	if got != want {
		t.Fatalf("project host url = %q, want %q", got, want)
	}
}

func TestSendWebTrackingPayload(t *testing.T) {
	var gotPayload webTrackingPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/logstores/usage-log/track" {
			t.Fatalf("path = %q, want /logstores/usage-log/track", r.URL.Path)
		}
		if got := r.Header.Get("x-log-apiversion"); got != defaultAPIVersion {
			t.Fatalf("x-log-apiversion = %q, want %q", got, defaultAPIVersion)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if rawSize := r.Header.Get("x-log-bodyrawsize"); rawSize == "" {
			t.Fatal("x-log-bodyrawsize missing")
		} else if _, err := strconv.Atoi(rawSize); err != nil {
			t.Fatalf("x-log-bodyrawsize invalid: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	plugin := &Plugin{
		cfg: Config{
			Topic:  "cliproxy_usage",
			Source: "cliproxy-test",
		},
		url:     server.URL + "/logstores/usage-log/track",
		enabled: true,
		client:  server.Client(),
	}
	if err := plugin.send(context.Background(), []map[string]string{{
		"event":        "usage",
		"provider":     "gemini",
		"total_tokens": "3",
	}}); err != nil {
		t.Fatalf("send error: %v", err)
	}
	if gotPayload.Topic != "cliproxy_usage" {
		t.Fatalf("topic = %q", gotPayload.Topic)
	}
	if gotPayload.Source != "cliproxy-test" {
		t.Fatalf("source = %q", gotPayload.Source)
	}
	if len(gotPayload.Logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(gotPayload.Logs))
	}
	if gotPayload.Logs[0]["provider"] != "gemini" || gotPayload.Logs[0]["total_tokens"] != "3" {
		t.Fatalf("unexpected log payload: %#v", gotPayload.Logs[0])
	}
}

func TestLogEntryFlattensUsageAndMasksAPIKey(t *testing.T) {
	plugin := &Plugin{}
	record := coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5-codex",
		Alias:       "codex-fast",
		APIKey:      "sk-1234567890",
		AuthID:      "auth-1",
		AuthIndex:   "2",
		AuthType:    "oauth",
		Source:      "user@example.com",
		RequestedAt: time.Date(2026, 5, 10, 1, 2, 3, 4, time.UTC),
		Latency:     150 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:     10,
			OutputTokens:    20,
			ReasoningTokens: 5,
			CachedTokens:    2,
		},
	}

	entry := plugin.logEntry(context.Background(), record)
	if entry["event"] != "usage" {
		t.Fatalf("event = %q, want usage", entry["event"])
	}
	if entry["provider"] != "codex" || entry["model"] != "gpt-5-codex" || entry["alias"] != "codex-fast" {
		t.Fatalf("unexpected model fields: %#v", entry)
	}
	if entry["api_key_fingerprint"] != "sk-1...7890" {
		t.Fatalf("api_key_fingerprint = %q", entry["api_key_fingerprint"])
	}
	if entry["total_tokens"] != "35" {
		t.Fatalf("total_tokens = %q, want 35", entry["total_tokens"])
	}
	if entry["failed"] != "false" || entry["status_code"] != "200" {
		t.Fatalf("unexpected success fields: failed=%q status=%q", entry["failed"], entry["status_code"])
	}
}
