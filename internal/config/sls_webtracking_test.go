package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptionalSLSWebTracking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte(`
port: 8317
sls-webtracking:
  enabled: true
  project: " test-project "
  logstore: " usage-log "
  endpoint: " cn-hangzhou.log.aliyuncs.com "
  topic: " usage "
  source: " node-a "
  queue-size: 2048
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional error: %v", err)
	}
	if cfg == nil {
		t.Fatal("config is nil")
	}
	if !cfg.SLSWebTracking.Enabled {
		t.Fatal("SLS WebTracking should be enabled")
	}
	if cfg.SLSWebTracking.Project != "test-project" {
		t.Fatalf("project = %q", cfg.SLSWebTracking.Project)
	}
	if cfg.SLSWebTracking.Logstore != "usage-log" {
		t.Fatalf("logstore = %q", cfg.SLSWebTracking.Logstore)
	}
	if cfg.SLSWebTracking.Endpoint != "cn-hangzhou.log.aliyuncs.com" {
		t.Fatalf("endpoint = %q", cfg.SLSWebTracking.Endpoint)
	}
	if cfg.SLSWebTracking.Topic != "usage" {
		t.Fatalf("topic = %q", cfg.SLSWebTracking.Topic)
	}
	if cfg.SLSWebTracking.Source != "node-a" {
		t.Fatalf("source = %q", cfg.SLSWebTracking.Source)
	}
	if cfg.SLSWebTracking.QueueSize != 2048 {
		t.Fatalf("queue-size = %d", cfg.SLSWebTracking.QueueSize)
	}
}

func TestParseConfigBytesSLSWebTrackingDefaultsDisabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes error: %v", err)
	}
	if cfg.SLSWebTracking.Enabled {
		t.Fatal("SLS WebTracking should be disabled by default")
	}
}
