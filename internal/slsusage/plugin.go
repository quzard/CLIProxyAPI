package slsusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultAPIVersion = "0.6.0"
	defaultTopic      = "cliproxy_usage"
	defaultQueueSize  = 1024
	maxFailureBodyLen = 4096
)

var (
	defaultMu         sync.Mutex
	defaultPlugin     *Plugin
	defaultRegister   sync.Once
	defaultRegisterMu sync.Mutex
)

// Config contains the Alibaba Cloud SLS WebTracking settings.
type Config struct {
	Enabled   bool
	Endpoint  string
	Project   string
	Logstore  string
	Topic     string
	Source    string
	QueueSize int
}

// Plugin forwards usage records to Alibaba Cloud SLS through WebTracking.
type Plugin struct {
	mu sync.RWMutex

	cfg     Config
	url     string
	enabled bool

	client *http.Client

	once sync.Once
	ch   chan map[string]string
}

// Configure registers or updates the process-wide SLS WebTracking usage plugin.
func Configure(cfg internalconfig.SLSWebTrackingConfig) error {
	pluginCfg := fromConfig(cfg)
	plugin := defaultUsagePlugin(pluginCfg)
	defaultRegisterMu.Lock()
	defaultRegister.Do(func() {
		coreusage.RegisterPlugin(plugin)
		plugin.Start()
	})
	defaultRegisterMu.Unlock()
	return plugin.UpdateConfig(pluginCfg)
}

func defaultUsagePlugin(initial Config) *Plugin {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultPlugin == nil {
		defaultPlugin = NewPlugin(initial)
	}
	return defaultPlugin
}

func fromConfig(cfg internalconfig.SLSWebTrackingConfig) Config {
	return Config{
		Enabled:   cfg.Enabled,
		Endpoint:  cfg.Endpoint,
		Project:   cfg.Project,
		Logstore:  cfg.Logstore,
		Topic:     cfg.Topic,
		Source:    cfg.Source,
		QueueSize: cfg.QueueSize,
	}
}

// NewPlugin creates an SLS WebTracking usage plugin.
func NewPlugin(cfg Config) *Plugin {
	cfg = normalizeConfig(cfg)
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	plugin := &Plugin{
		client: http.DefaultClient,
		ch:     make(chan map[string]string, queueSize),
	}
	_ = plugin.UpdateConfig(cfg)
	return plugin
}

// UpdateConfig updates the active WebTracking configuration.
func (p *Plugin) UpdateConfig(cfg Config) error {
	if p == nil {
		return nil
	}
	cfg = normalizeConfig(cfg)
	if !cfg.Enabled {
		p.mu.Lock()
		p.cfg = cfg
		p.url = ""
		p.enabled = false
		p.mu.Unlock()
		return nil
	}
	if cfg.Project == "" {
		p.disable(cfg)
		return errors.New("sls-webtracking.project is required when SLS WebTracking is enabled")
	}
	if cfg.Logstore == "" {
		p.disable(cfg)
		return errors.New("sls-webtracking.logstore is required when SLS WebTracking is enabled")
	}
	if cfg.Endpoint == "" {
		p.disable(cfg)
		return errors.New("sls-webtracking.endpoint is required when SLS WebTracking is enabled")
	}
	trackURL, err := buildTrackURL(cfg)
	if err != nil {
		p.disable(cfg)
		return err
	}

	p.mu.Lock()
	changed := p.url != trackURL || !p.enabled
	p.cfg = cfg
	p.url = trackURL
	p.enabled = true
	p.mu.Unlock()

	if changed {
		log.WithFields(log.Fields{
			"project":  cfg.Project,
			"logstore": cfg.Logstore,
			"endpoint": cfg.Endpoint,
		}).Info("SLS WebTracking usage export enabled")
	}
	return nil
}

func (p *Plugin) disable(cfg Config) {
	p.mu.Lock()
	p.cfg = cfg
	p.url = ""
	p.enabled = false
	p.mu.Unlock()
}

// Start launches the background sender.
func (p *Plugin) Start() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		go p.run()
	})
}

// HandleUsage converts a usage record into a flat WebTracking log.
func (p *Plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	if !p.isEnabled() {
		return
	}
	p.Start()
	entry := p.logEntry(ctx, record)
	select {
	case p.ch <- entry:
	default:
		log.Debug("sls webtracking usage queue full; dropping usage record")
	}
}

func (p *Plugin) isEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.enabled
}

func (p *Plugin) run() {
	for entry := range p.ch {
		if err := p.send(context.Background(), []map[string]string{entry}); err != nil {
			log.WithError(err).Warn("failed to send usage record to SLS WebTracking")
		}
	}
}

func (p *Plugin) send(ctx context.Context, logs []map[string]string) error {
	if p == nil || len(logs) == 0 {
		return nil
	}
	p.mu.RLock()
	cfg := p.cfg
	trackURL := p.url
	enabled := p.enabled
	client := p.client
	p.mu.RUnlock()
	if !enabled {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}

	body, err := json.Marshal(webTrackingPayload{
		Topic:  cfg.Topic,
		Source: cfg.Source,
		Logs:   logs,
	})
	if err != nil {
		return fmt.Errorf("marshal webtracking payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trackURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create webtracking request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-log-apiversion", defaultAPIVersion)
	req.Header.Set("x-log-bodyrawsize", strconv.Itoa(len(body)))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post webtracking request: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close SLS WebTracking response body")
		}
	}()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxFailureBodyLen))
	return fmt.Errorf("webtracking status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}

func (p *Plugin) logEntry(ctx context.Context, record coreusage.Record) map[string]string {
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	modelName := trimDefault(record.Model, "unknown")
	aliasName := strings.TrimSpace(record.Alias)
	if aliasName == "" {
		aliasName = modelName
	}
	provider := trimDefault(record.Provider, "unknown")
	authType := trimDefault(record.AuthType, "unknown")
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	endpoint := strings.TrimSpace(internallogging.GetEndpoint(ctx))

	inputTokens := record.Detail.InputTokens
	outputTokens := record.Detail.OutputTokens
	reasoningTokens := record.Detail.ReasoningTokens
	cachedTokens := record.Detail.CachedTokens
	totalTokens := record.Detail.TotalTokens
	if totalTokens == 0 {
		totalTokens = inputTokens + outputTokens + reasoningTokens
	}
	if totalTokens == 0 {
		totalTokens = inputTokens + outputTokens + reasoningTokens + cachedTokens
	}

	failed := record.Failed
	statusCode := record.Fail.StatusCode
	if !failed {
		responseStatus := internallogging.GetResponseStatus(ctx)
		if responseStatus != 0 {
			failed = responseStatus >= http.StatusBadRequest
			if statusCode == 0 {
				statusCode = responseStatus
			}
		}
	}
	if statusCode == 0 {
		if failed {
			statusCode = http.StatusInternalServerError
		} else {
			statusCode = http.StatusOK
		}
	}

	return map[string]string{
		"event":               "usage",
		"request_time":        timestamp.UTC().Format(time.RFC3339Nano),
		"request_id":          requestID,
		"provider":            provider,
		"model":               modelName,
		"alias":               aliasName,
		"endpoint":            endpoint,
		"source":              strings.TrimSpace(record.Source),
		"auth_id":             strings.TrimSpace(record.AuthID),
		"auth_index":          strings.TrimSpace(record.AuthIndex),
		"auth_type":           authType,
		"api_key_fingerprint": apiKeyFingerprint(record.APIKey),
		"latency_ms":          strconv.FormatInt(record.Latency.Milliseconds(), 10),
		"failed":              strconv.FormatBool(failed),
		"status_code":         strconv.Itoa(statusCode),
		"failure_body":        truncate(strings.TrimSpace(record.Fail.Body), maxFailureBodyLen),
		"input_tokens":        strconv.FormatInt(inputTokens, 10),
		"output_tokens":       strconv.FormatInt(outputTokens, 10),
		"reasoning_tokens":    strconv.FormatInt(reasoningTokens, 10),
		"cached_tokens":       strconv.FormatInt(cachedTokens, 10),
		"total_tokens":        strconv.FormatInt(totalTokens, 10),
	}
}

type webTrackingPayload struct {
	Topic  string              `json:"__topic__"`
	Source string              `json:"__source__"`
	Logs   []map[string]string `json:"__logs__"`
}

func normalizeConfig(cfg Config) Config {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Project = strings.TrimSpace(cfg.Project)
	cfg.Logstore = strings.TrimSpace(cfg.Logstore)
	cfg.Topic = strings.TrimSpace(cfg.Topic)
	cfg.Source = strings.TrimSpace(cfg.Source)
	if cfg.Topic == "" {
		cfg.Topic = defaultTopic
	}
	if cfg.Source == "" {
		cfg.Source = defaultSource()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	return cfg
}

func buildTrackURL(cfg Config) (string, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return "", errors.New("SLS WebTracking endpoint is empty")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse SLS WebTracking endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported SLS WebTracking endpoint scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("SLS WebTracking endpoint host is empty")
	}
	host := parsed.Host
	projectPrefix := cfg.Project + "."
	if !strings.HasPrefix(strings.ToLower(host), strings.ToLower(projectPrefix)) {
		host = cfg.Project + "." + host
	}
	parsed.Host = host
	parsed.Path = "/logstores/" + url.PathEscape(cfg.Logstore) + "/track"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func defaultSource() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "cliproxyapi"
	}
	return strings.TrimSpace(host)
}

func trimDefault(value, def string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return def
	}
	return value
}

func truncate(value string, maxLen int) string {
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}

func apiKeyFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return strings.Repeat("*", len(value))
	}
	return value[:4] + "..." + value[len(value)-4:]
}
