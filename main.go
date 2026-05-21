package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// -----------------------------------------------------------------------------
// Custom JSON unmarshalers
// -----------------------------------------------------------------------------

// StringOrBool is a custom type that unmarshals a JSON value that may be either
// a string, a bool, or null.
type StringOrBool string

// UnmarshalJSON implements the json.Unmarshaler interface for StringOrBool.
func (s *StringOrBool) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = "unknown"
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var tmp string
		if err := json.Unmarshal(b, &tmp); err != nil {
			return err
		}
		*s = StringOrBool(tmp)
		return nil
	}
	var tmp bool
	if err := json.Unmarshal(b, &tmp); err == nil {
		if tmp {
			*s = "true"
		} else {
			*s = "false"
		}
		return nil
	}
	return fmt.Errorf("unsupported type for StringOrBool: %s", string(b))
}

// FloatOrString unmarshals a JSON number or a JSON string containing a number.
type FloatOrString float64

func (f *FloatOrString) UnmarshalJSON(data []byte) error {
	var floatVal float64
	if err := json.Unmarshal(data, &floatVal); err == nil {
		*f = FloatOrString(floatVal)
		return nil
	}

	var strVal string
	if err := json.Unmarshal(data, &strVal); err != nil {
		return err
	}

	floatVal, err := strconv.ParseFloat(strVal, 64)
	if err != nil {
		return err
	}

	*f = FloatOrString(floatVal)
	return nil
}

// -----------------------------------------------------------------------------
// Global state
// -----------------------------------------------------------------------------

var (
	stateMu sync.RWMutex
	// usageState stores already processed buckets to avoid double counting.
	usageState   = make(map[string]float64)
	projectNames = make(map[string]string) // mapping project_id -> project_name
	apiKeyNames  = make(map[string]string) // mapping api_key_id -> api_key_name

	// lastUsageScrape is the unix-timestamp the next usage scrape window starts from.
	// Stored atomically so it can be safely read/written from multiple goroutines.
	lastUsageScrape atomic.Int64

	// nameLookupGroup deduplicates concurrent name lookups for the same id.
	nameLookupGroup singleflight.Group

	// readiness flips true after the first successful usage scrape.
	ready atomic.Bool
)

// -----------------------------------------------------------------------------
// Configuration / flags
// -----------------------------------------------------------------------------

type UsageEndpoint struct {
	Path string // API endpoint path (e.g. "completions")
	Name string // Operation name (e.g. "completions")
}

var (
	listenAddress = flag.String("web.listen-address", ":9185", "Address to listen on for web interface and telemetry")
	metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics")

	scrapeInterval = flag.Duration("scrape.interval", 1*time.Minute, "Interval between usage API scrapes (also the bucket window size)")
	costInterval   = flag.Duration("cost.interval", 1*time.Hour, "Interval between cost API scrapes")
	costLookback   = flag.Duration("cost.lookback", 35*24*time.Hour, "How far back to fetch daily cost data on each cost scrape")
	usageBackfill  = flag.Duration("usage.backfill", 1*time.Hour, "On startup, how far back to fetch usage data")

	logLevel  = flag.String("log.level", "info", "Log level (debug, info, warn, error)")
	logFormat = flag.String("log.format", "text", "Log format (text or json)")

	// Cardinality controls
	labelUserID   = flag.Bool("label.user_id", true, "Include user_id label on token metrics")
	labelAPIKeyID = flag.Bool("label.api_key_id", true, "Include api_key_id and api_key_name labels on token metrics")

	// OpenAI-Organization header is optional. If set, it's added to outgoing requests.
	openAIOrgHeader = flag.String("openai.org-header", "", "Optional OpenAI-Organization header value (defaults to $OPENAI_ORG_ID if set)")

	// Token-based usage endpoints. We deliberately exclude vector_stores,
	// audio_speeches and audio_transcriptions from token tracking since they
	// don't return token counts (vector_stores returns bytes; audio returns
	// characters/seconds). Cost for these still shows up in /costs.
	usageEndpoints = []UsageEndpoint{
		{Path: "completions", Name: "completions"},
		{Path: "embeddings", Name: "embeddings"},
		{Path: "moderations", Name: "moderations"},
		{Path: "images", Name: "images"},
	}

	// Prometheus metrics. Label sets are decided at registration time, but the
	// optional label values default to "" when the corresponding flag is off so
	// the schema stays stable.
	tokensTotal *prometheus.CounterVec
	dailyCost   *prometheus.GaugeVec

	// Operational metrics for the exporter itself.
	scrapeErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openai_exporter_scrape_errors_total",
			Help: "Number of errors encountered when scraping the OpenAI API, by endpoint.",
		},
		[]string{"endpoint"},
	)
	lastSuccessTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openai_exporter_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful scrape, by scrape kind (usage|cost).",
		},
		[]string{"kind"},
	)
	scrapeDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "openai_exporter_scrape_duration_seconds",
			Help:    "Duration of scrape operations in seconds, by scrape kind.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		},
		[]string{"kind"},
	)
)

// labelNames returns the label set used for tokensTotal, honoring cardinality flags.
func tokenLabelNames() []string {
	names := []string{"model", "operation", "project_id", "project_name", "batch", "token_type"}
	if *labelUserID {
		names = append(names, "user_id")
	}
	if *labelAPIKeyID {
		names = append(names, "api_key_id", "api_key_name")
	}
	return names
}

func registerMetrics() {
	tokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openai_api_tokens_total",
			Help: "Total number of tokens used per model, operation, project, batch and token type.",
		},
		tokenLabelNames(),
	)
	dailyCost = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openai_api_daily_cost",
			Help: "Daily spend by date/project/line_item/organization. Currency is exposed as a label.",
		},
		[]string{"date", "project_id", "project_name", "line_item", "organization_id", "currency"},
	)
	prometheus.MustRegister(tokensTotal, dailyCost, scrapeErrorsTotal, lastSuccessTimestamp, scrapeDurationSeconds)
}

func setupLogging() {
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse log level")
	}
	logrus.SetLevel(level)
	switch *logFormat {
	case "json":
		logrus.SetFormatter(&logrus.JSONFormatter{})
	default:
		logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	}
	logrus.Infof("Log level set to %s", level)
}

// -----------------------------------------------------------------------------
// Exporter / API types
// -----------------------------------------------------------------------------

type Exporter struct {
	client    *http.Client
	apiKey    string
	orgHeader string
}

type APIResponse struct {
	Object   string   `json:"object"`
	Data     []Bucket `json:"data"`
	HasMore  bool     `json:"has_more"`
	NextPage string   `json:"next_page"`
}

type Bucket struct {
	Object    string        `json:"object"`
	StartTime int64         `json:"start_time"`
	EndTime   int64         `json:"end_time"`
	Results   []UsageResult `json:"results"`
}

type UsageResult struct {
	Object            string       `json:"object"`
	InputTokens       int64        `json:"input_tokens"`
	OutputTokens      int64        `json:"output_tokens"`
	InputCachedTokens int64        `json:"input_cached_tokens"`
	InputAudioTokens  int64        `json:"input_audio_tokens"`
	OutputAudioTokens int64        `json:"output_audio_tokens"`
	NumModelRequests  int64        `json:"num_model_requests"`
	ProjectID         *string      `json:"project_id"`
	UserID            *string      `json:"user_id"`
	APIKeyID          *string      `json:"api_key_id"`
	Model             *string      `json:"model"`
	Batch             StringOrBool `json:"batch"`
}

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ProjectsList struct {
	Object   string    `json:"object"`
	Data     []Project `json:"data"`
	HasMore  bool      `json:"has_more"`
	LastID   string    `json:"last_id"`
	FirstID  string    `json:"first_id"`
	NextPage string    `json:"next_page"`
}

type APIKey struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type CostsList struct {
	Object   string       `json:"object"`
	Data     []CostBucket `json:"data"`
	HasMore  bool         `json:"has_more"`
	NextPage string       `json:"next_page"`
}

type CostBucket struct {
	Object    string       `json:"object"`
	StartTime int64        `json:"start_time"`
	EndTime   int64        `json:"end_time"`
	Results   []CostResult `json:"results"`
}

type Money struct {
	Value    FloatOrString `json:"value"`
	Currency string        `json:"currency"`
}

type CostResult struct {
	Object         string  `json:"object"`
	Amount         Money   `json:"amount"`
	LineItem       *string `json:"line_item"`
	ProjectID      *string `json:"project_id"`
	OrganizationID string  `json:"organization_id"`
}

func NewExporter() (*Exporter, error) {
	apiKey := os.Getenv("OPENAI_SECRET_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_SECRET_KEY environment variable is not set")
	}

	// OPENAI_ORG_ID / openai.org-header are optional. The OpenAI admin API
	// is org-scoped via the admin key, so the header is only needed for
	// keys that span multiple orgs.
	orgHeader := *openAIOrgHeader
	if orgHeader == "" {
		orgHeader = os.Getenv("OPENAI_ORG_ID")
	}

	return &Exporter{
		client:    &http.Client{Timeout: 30 * time.Second},
		apiKey:    apiKey,
		orgHeader: orgHeader,
	}, nil
}

// newRequest builds an HTTP request with auth and (optional) org headers attached.
func (e *Exporter) newRequest(ctx context.Context, method, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	if e.orgHeader != "" {
		req.Header.Set("OpenAI-Organization", e.orgHeader)
	}
	return req, nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func mergeLabels(base prometheus.Labels, key, value string) prometheus.Labels {
	out := make(prometheus.Labels, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[key] = value
	return out
}

// updateMetric updates a counter for a given token type. It only emits values
// for completed buckets (bucketEnd <= now) and uses usageState to dedup buckets
// already seen.
func updateMetric(labels prometheus.Labels, tokenType string, bucketStart, bucketEnd int64, newValue float64) {
	compositeKey := strings.Join([]string{
		labels["operation"],
		fmt.Sprintf("%d", bucketStart),
		labels["project_id"],
		labels["user_id"],
		labels["api_key_id"],
		labels["model"],
		labels["batch"],
		tokenType,
	}, "|")

	now := time.Now().Unix()
	if bucketEnd > now {
		logrus.Debugf("Bucket %s not yet completed (bucketEnd: %d, now: %d), skipping", compositeKey, bucketEnd, now)
		return
	}

	stateMu.Lock()
	defer stateMu.Unlock()

	if _, exists := usageState[compositeKey]; exists {
		return
	}

	tokensTotal.With(mergeLabels(labels, "token_type", tokenType)).Add(newValue)
	usageState[compositeKey] = newValue
}

func deref(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}

// -----------------------------------------------------------------------------
// Usage data collection
// -----------------------------------------------------------------------------

func (e *Exporter) fetchUsageData(ctx context.Context, endpoint UsageEndpoint, startTime, endTime int64) error {
	baseURL := fmt.Sprintf("https://api.openai.com/v1/organization/usage/%s", endpoint.Path)
	nextPage := ""
	total := 0

	for {
		groupBy := "project_id,model,batch"
		if *labelUserID {
			groupBy += ",user_id"
		}
		if *labelAPIKeyID {
			groupBy += ",api_key_id"
		}
		url := fmt.Sprintf("%s?start_time=%d&end_time=%d&bucket_width=1m&limit=1440&group_by=%s",
			baseURL, startTime, endTime, groupBy)
		if nextPage != "" {
			url += "&page=" + nextPage
		}

		logrus.Debugf("Fetching usage data: %s", url)

		req, err := e.newRequest(ctx, http.MethodGet, url)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := e.client.Do(req)
		if err != nil {
			return fmt.Errorf("error fetching usage data: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			return fmt.Errorf("usage API returned %d for %s: %s", resp.StatusCode, endpoint.Path, strings.TrimSpace(string(body)))
		}

		var response APIResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&response)
		closeErr := resp.Body.Close()

		if decodeErr != nil {
			return fmt.Errorf("error decoding response: %w", decodeErr)
		}
		if closeErr != nil {
			logrus.WithError(closeErr).Warn("failed to close response body")
		}

		for _, bucket := range response.Data {
			for _, result := range bucket.Results {
				total++

				projectID := deref(result.ProjectID)
				labels := prometheus.Labels{
					"model":        deref(result.Model),
					"operation":    endpoint.Name,
					"project_id":   projectID,
					"project_name": e.ensureProjectName(ctx, projectID),
					"batch":        string(result.Batch),
				}
				if *labelUserID {
					labels["user_id"] = deref(result.UserID)
				}
				if *labelAPIKeyID {
					apiKeyID := deref(result.APIKeyID)
					labels["api_key_id"] = apiKeyID
					labels["api_key_name"] = e.ensureAPIKeyName(ctx, projectID, apiKeyID)
				}

				updateMetric(labels, "input", bucket.StartTime, bucket.EndTime, float64(result.InputTokens))
				updateMetric(labels, "output", bucket.StartTime, bucket.EndTime, float64(result.OutputTokens))
				updateMetric(labels, "input_cached", bucket.StartTime, bucket.EndTime, float64(result.InputCachedTokens))
				updateMetric(labels, "input_audio", bucket.StartTime, bucket.EndTime, float64(result.InputAudioTokens))
				updateMetric(labels, "output_audio", bucket.StartTime, bucket.EndTime, float64(result.OutputAudioTokens))
			}
		}

		if !response.HasMore {
			break
		}
		nextPage = response.NextPage
	}

	logrus.Debugf("Total records fetched from %s: %d", endpoint.Path, total)
	return nil
}

// -----------------------------------------------------------------------------
// Project / API key name resolution (cached + singleflight)
// -----------------------------------------------------------------------------

// ensureProjectName returns the cached human-readable name for a project, or
// resolves it via the API. It uses singleflight to avoid duplicate concurrent
// lookups, and never caches negative results.
func (e *Exporter) ensureProjectName(ctx context.Context, projectID string) string {
	if projectID == "" || projectID == "unknown" {
		return "unknown"
	}

	stateMu.RLock()
	if n, ok := projectNames[projectID]; ok && n != "" {
		stateMu.RUnlock()
		return n
	}
	stateMu.RUnlock()

	v, _, _ := nameLookupGroup.Do("project:"+projectID, func() (any, error) {
		// Re-check after entering singleflight: another goroutine may have populated it.
		stateMu.RLock()
		if n, ok := projectNames[projectID]; ok && n != "" {
			stateMu.RUnlock()
			return n, nil
		}
		stateMu.RUnlock()

		url := fmt.Sprintf("https://api.openai.com/v1/organization/projects/%s", projectID)
		req, err := e.newRequest(ctx, http.MethodGet, url)
		if err != nil {
			return "unknown", nil
		}

		resp, err := e.client.Do(req)
		if err != nil {
			return "unknown", nil
		}
		defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "unknown", nil
		}

		var obj Project
		if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil || obj.Name == "" {
			return "unknown", nil
		}

		stateMu.Lock()
		projectNames[projectID] = obj.Name
		stateMu.Unlock()
		return obj.Name, nil
	})

	if name, ok := v.(string); ok {
		return name
	}
	return "unknown"
}

// preloadProjects best-effort lists all projects so that name resolution
// during scrape is a cache hit. Failure is logged but not fatal.
func (e *Exporter) preloadProjects(ctx context.Context) {
	url := "https://api.openai.com/v1/organization/projects?limit=100"
	count := 0
	for {
		req, err := e.newRequest(ctx, http.MethodGet, url)
		if err != nil {
			logrus.WithError(err).Warn("preloadProjects: failed to create request")
			return
		}
		resp, err := e.client.Do(req)
		if err != nil {
			logrus.WithError(err).Warn("preloadProjects: request failed")
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			logrus.Warnf("preloadProjects: API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			return
		}
		var list ProjectsList
		decodeErr := json.NewDecoder(resp.Body).Decode(&list)
		_ = resp.Body.Close()
		if decodeErr != nil {
			logrus.WithError(decodeErr).Warn("preloadProjects: decode failed")
			return
		}
		stateMu.Lock()
		for _, p := range list.Data {
			if p.ID != "" && p.Name != "" {
				projectNames[p.ID] = p.Name
				count++
			}
		}
		stateMu.Unlock()
		if !list.HasMore || list.LastID == "" {
			break
		}
		url = fmt.Sprintf("https://api.openai.com/v1/organization/projects?limit=100&after=%s", list.LastID)
	}
	logrus.Infof("preloadProjects: cached %d project names", count)
}

func (e *Exporter) ensureAPIKeyName(ctx context.Context, projectID, apiKeyID string) string {
	if apiKeyID == "" || apiKeyID == "unknown" {
		return "unknown"
	}

	stateMu.RLock()
	if n, ok := apiKeyNames[apiKeyID]; ok && n != "" {
		stateMu.RUnlock()
		return n
	}
	stateMu.RUnlock()

	v, _, _ := nameLookupGroup.Do("apikey:"+apiKeyID, func() (any, error) {
		stateMu.RLock()
		if n, ok := apiKeyNames[apiKeyID]; ok && n != "" {
			stateMu.RUnlock()
			return n, nil
		}
		stateMu.RUnlock()

		var urls []string
		if projectID != "" && projectID != "unknown" {
			urls = append(urls, fmt.Sprintf("https://api.openai.com/v1/organization/projects/%s/api_keys/%s", projectID, apiKeyID))
		}
		urls = append(urls, fmt.Sprintf("https://api.openai.com/v1/organization/api_keys/%s", apiKeyID))

		for _, u := range urls {
			req, err := e.newRequest(ctx, http.MethodGet, u)
			if err != nil {
				continue
			}
			resp, err := e.client.Do(req)
			if err != nil {
				continue
			}
			func() {
				defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					return
				}
				var obj APIKey
				if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil || obj.Name == "" {
					return
				}
				stateMu.Lock()
				apiKeyNames[apiKeyID] = obj.Name
				stateMu.Unlock()
			}()

			stateMu.RLock()
			if n, ok := apiKeyNames[apiKeyID]; ok && n != "" {
				stateMu.RUnlock()
				return n, nil
			}
			stateMu.RUnlock()
		}
		return "unknown", nil
	})

	if name, ok := v.(string); ok {
		return name
	}
	return "unknown"
}

// -----------------------------------------------------------------------------
// Cost data collection
// -----------------------------------------------------------------------------

// fetchCostData fetches daily-bucketed cost data for the requested window and
// re-publishes it. Because cost is restated by OpenAI as the day finishes,
// the gauge is reset before each scrape so stale series drop out.
func (e *Exporter) fetchCostData(ctx context.Context, startTime, endTime int64) error {
	baseURL := "https://api.openai.com/v1/organization/costs"
	nextPage := ""

	// Stale series removal: reset the entire vector before re-publishing.
	dailyCost.Reset()

	for {
		url := fmt.Sprintf("%s?start_time=%d&end_time=%d&bucket_width=1d&limit=180&group_by=project_id,line_item",
			baseURL, startTime, endTime)
		if nextPage != "" {
			url += "&page=" + nextPage
		}

		logrus.Debugf("Fetching cost data: %s", url)

		req, err := e.newRequest(ctx, http.MethodGet, url)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := e.client.Do(req)
		if err != nil {
			return fmt.Errorf("error fetching cost data: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			return fmt.Errorf("cost API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var out CostsList
		decodeErr := json.NewDecoder(resp.Body).Decode(&out)
		closeErr := resp.Body.Close()

		if decodeErr != nil {
			return fmt.Errorf("error decoding response: %w", decodeErr)
		}
		if closeErr != nil {
			logrus.WithError(closeErr).Warn("failed to close response body")
		}

		for _, bucket := range out.Data {
			date := time.Unix(bucket.StartTime, 0).UTC().Format("2006-01-02")
			for _, res := range bucket.Results {
				projectID := deref(res.ProjectID)
				lineName := "unknown"
				if res.LineItem != nil && *res.LineItem != "" {
					lineName = *res.LineItem
				}
				labels := prometheus.Labels{
					"date":            date,
					"project_id":      projectID,
					"project_name":    e.ensureProjectName(ctx, projectID),
					"line_item":       lineName,
					"organization_id": res.OrganizationID,
					"currency":        res.Amount.Currency,
				}
				dailyCost.With(labels).Set(float64(res.Amount.Value))
			}
		}

		if !out.HasMore {
			break
		}
		nextPage = out.NextPage
	}

	return nil
}

// -----------------------------------------------------------------------------
// Scrape loops
// -----------------------------------------------------------------------------

func (e *Exporter) usageScrapeOnce(ctx context.Context) {
	step := int64((*scrapeInterval) / time.Second)
	if step <= 0 {
		step = 60
	}

	startTime := lastUsageScrape.Load()
	endTime := startTime + step

	timer := prometheus.NewTimer(scrapeDurationSeconds.WithLabelValues("usage"))
	defer timer.ObserveDuration()

	logrus.Debugf("Starting usage scrape: startTime=%d endTime=%d", startTime, endTime)

	var wg sync.WaitGroup
	var anyErr atomic.Bool
	for _, endpoint := range usageEndpoints {
		wg.Add(1)
		go func(ep UsageEndpoint) {
			defer wg.Done()
			if err := e.fetchUsageData(ctx, ep, startTime, endTime); err != nil {
				logrus.WithError(err).Errorf("Error fetching usage from %s", ep.Path)
				scrapeErrorsTotal.WithLabelValues("usage:" + ep.Path).Inc()
				anyErr.Store(true)
			}
		}(endpoint)
	}
	wg.Wait()

	lastUsageScrape.Store(endTime)
	if !anyErr.Load() {
		lastSuccessTimestamp.WithLabelValues("usage").Set(float64(time.Now().Unix()))
		ready.Store(true)
	}
}

func (e *Exporter) costScrapeOnce(ctx context.Context) {
	timer := prometheus.NewTimer(scrapeDurationSeconds.WithLabelValues("cost"))
	defer timer.ObserveDuration()

	now := time.Now().UTC()
	end := now.Add(24 * time.Hour).Unix()                         // include today
	start := now.Add(-(*costLookback)).Truncate(time.Hour).Unix() // start of lookback window

	if err := e.fetchCostData(ctx, start, end); err != nil {
		logrus.WithError(err).Warn("Error fetching cost data")
		scrapeErrorsTotal.WithLabelValues("cost").Inc()
		return
	}
	lastSuccessTimestamp.WithLabelValues("cost").Set(float64(time.Now().Unix()))
}

func (e *Exporter) runUsageLoop(ctx context.Context) {
	ticker := time.NewTicker(*scrapeInterval)
	defer ticker.Stop()
	// First scrape immediately.
	e.usageScrapeOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.usageScrapeOnce(ctx)
		}
	}
}

func (e *Exporter) runCostLoop(ctx context.Context) {
	ticker := time.NewTicker(*costInterval)
	defer ticker.Stop()
	e.costScrapeOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.costScrapeOnce(ctx)
		}
	}
}

// -----------------------------------------------------------------------------
// HTTP handlers
// -----------------------------------------------------------------------------

func indexHandler(w http.ResponseWriter, _ *http.Request) {
	_, err := w.Write([]byte("<html><head><title>OpenAI Exporter</title></head>" +
		"<body><h1>OpenAI Exporter</h1>" +
		"<p><a href='" + *metricsPath + "'>Metrics</a></p>" +
		"<p><a href='/healthz'>Liveness</a> | <a href='/readyz'>Readiness</a></p>" +
		"</body></html>"))
	if err != nil {
		logrus.WithError(err).Error("Failed to write response")
	}
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyzHandler(w http.ResponseWriter, _ *http.Request) {
	if ready.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("not ready"))
}

// -----------------------------------------------------------------------------
// main
// -----------------------------------------------------------------------------

func main() {
	flag.Parse()
	setupLogging()
	registerMetrics()

	// Initial usage scrape window: now - usageBackfill, rounded down to a minute.
	startWindow := time.Now().Add(-*usageBackfill).Round(time.Minute).Unix()
	lastUsageScrape.Store(startWindow)
	logrus.Infof("Usage backfill from t=%d (%s)", startWindow, time.Unix(startWindow, 0).UTC().Format(time.RFC3339))

	exporter, err := NewExporter()
	if err != nil {
		logrus.Fatal(err)
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Pre-warm project names. Best-effort; errors are non-fatal.
	go exporter.preloadProjects(rootCtx)

	go exporter.runUsageLoop(rootCtx)
	go exporter.runCostLoop(rootCtx)

	mux := http.NewServeMux()
	mux.Handle(*metricsPath, promhttp.Handler())
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandler)

	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-rootCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logrus.Infof("Starting server on %s", *listenAddress)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logrus.Fatal(err)
	}
}
