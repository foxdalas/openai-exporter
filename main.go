package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// Custom Type for Batch Field

// StringOrBool is a custom type that unmarshals a JSON value that may be either a string, a bool, or null.
type StringOrBool string

// UnmarshalJSON implements the json.Unmarshaler interface for StringOrBool.
func (s *StringOrBool) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = "unknown"
		return nil
	}
	if b[0] == '"' {
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

// Global Variables and State

var (
	stateMu sync.RWMutex
	// usageState stores already processed buckets to avoid double counting.
	usageState   = make(map[string]float64)
	lastScrape   = int64(0)
	projectNames = make(map[string]string) // mapping project_id -> project_name
	apiKeyNames  = make(map[string]string) // mapping api_key_id -> api_key_name
)

// Prometheus Metric and CLI Flags

type UsageEndpoint struct {
	Path string // API endpoint path (e.g. "completions")
	Name string // Name of the operation (e.g. "completions")
}

var (
	listenAddress = flag.String("web.listen-address", ":9185", "Address to listen on for web interface and telemetry")
	metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics")
	// API polling interval; also used to determine the time window (last minute).
	scrapeInterval = flag.Duration("scrape.interval", 1*time.Minute, "Interval for API calls and data window")
	logLevel       = flag.String("log.level", "info", "Log level")

	usageEndpoints = []UsageEndpoint{
		{Path: "completions", Name: "completions"},
		{Path: "embeddings", Name: "embeddings"},
		{Path: "moderations", Name: "moderations"},
		{Path: "images", Name: "images"},
		{Path: "audio_speeches", Name: "audio_speeches"},
		{Path: "audio_transcriptions", Name: "audio_transcriptions"},
		{Path: "vector_stores", Name: "vector_stores"},
	}

	tokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openai_api_tokens_total",
			Help: "Total number of tokens used per model, operation, project, user, API key, batch and token type",
		},
		[]string{"model", "operation", "project_id", "project_name", "user_id", "api_key_id", "api_key_name", "batch", "token_type"},
	)
	dailyCostUSD = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "openai_api_daily_cost",
			Help: "Daily spend by date/project/line_item/organization (currency indicated by label).",
		},
		[]string{"date", "project_id", "project_name", "line_item", "organization_id", "currency"},
	)
)

func init() {
	prometheus.MustRegister(tokensTotal)
	prometheus.MustRegister(dailyCostUSD)
}

func setupLogging() {
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse log level")
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	logrus.Infof("Log level set to %s", level)
}

// Exporter and API Structures

type Exporter struct {
	client *http.Client
	apiKey string
	orgID  string
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
	Name string `json:"name"`
}

type APIKey struct {
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
	Value    float64 `json:"value"`
	Currency string  `json:"currency"`
}

func (m *Money) UnmarshalJSON(b []byte) error {
	var aux struct {
		Value    interface{} `json:"value"`
		Currency string      `json:"currency"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}

	switch v := aux.Value.(type) {
	case nil:
		m.Value = 0
	case float64:
		m.Value = v
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("cannot parse Money.value (string): %w", err)
		}
		m.Value = f
	default:
		return fmt.Errorf("unexpected type for Money.value: %T", v)
	}
	m.Currency = aux.Currency
	return nil
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
	orgID := os.Getenv("OPENAI_ORG_ID")
	if orgID == "" {
		return nil, fmt.Errorf("OPENAI_ORG_ID environment variable is not set")
	}
	return &Exporter{
		client: &http.Client{Timeout: 10 * time.Second},
		apiKey: apiKey,
		orgID:  orgID,
	}, nil
}

// Helper Functions for State and Metrics

func mergeLabels(base prometheus.Labels, key, value string) prometheus.Labels {
	newLabels := make(prometheus.Labels, len(base)+1)
	for k, v := range base {
		newLabels[k] = v
	}
	newLabels[key] = value
	return newLabels
}

// updateMetric updates the metric for a given token type.
// If the bucket is completed (bucketEnd <= current time) and has not been processed yet,
// its value is added to the counter, and the bucket information is saved in usageState.
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
	// Update the metric only if the bucket is completed.
	if bucketEnd > now {
		logrus.Debugf("Bucket %s is not yet completed (bucketEnd: %d, now: %d), skipping", compositeKey, bucketEnd, now)
		return
	}

	stateMu.Lock()
	defer stateMu.Unlock()

	// If the bucket has already been processed, it is not updated again.
	if _, exists := usageState[compositeKey]; exists {
		logrus.Debugf("Bucket %s has already been processed, skipping", compositeKey)
		return
	}

	tokensTotal.With(mergeLabels(labels, "token_type", tokenType)).Add(newValue)
	usageState[compositeKey] = newValue
}

// Data Collection

func (e *Exporter) fetchUsageData(endpoint UsageEndpoint, startTime, endTime int64) error {
	baseURL := fmt.Sprintf("https://api.openai.com/v1/organization/usage/%s", endpoint.Path)
	nextPage := ""

	allResults := []UsageResult{}

	for {
		url := fmt.Sprintf("%s?start_time=%d&end_time=%d&bucket_width=1m&limit=1440&group_by=project_id,user_id,api_key_id,model,batch",
			baseURL, startTime, endTime)
		if nextPage != "" {
			url += "&page=" + nextPage
		}

		logrus.Debugf("Fetching usage data: %s", url)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)

		resp, err := e.client.Do(req)
		if err != nil {
			return fmt.Errorf("error fetching usage data: %w", err)
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
		logrus.Debugf("Received response: %+v", response)

		for _, bucket := range response.Data {
			if len(bucket.Results) > 0 {
				logrus.Debugf("Results %+v", bucket.Results)
			}
			for _, result := range bucket.Results {
				allResults = append(allResults, result)

				labels := prometheus.Labels{
					"model":        deref(result.Model),
					"operation":    endpoint.Name,
					"project_id":   deref(result.ProjectID),
					"project_name": e.ensureProjectName(deref(result.ProjectID)),
					"user_id":      deref(result.UserID),
					"api_key_id":   deref(result.APIKeyID),
					"api_key_name": e.ensureAPIKeyName(deref(result.ProjectID), deref(result.APIKeyID)),
					"batch":        string(result.Batch),
				}

				updateMetric(labels, "input", bucket.StartTime, bucket.EndTime, float64(result.InputTokens))
				updateMetric(labels, "output", bucket.StartTime, bucket.EndTime, float64(result.OutputTokens))
				updateMetric(labels, "input_cached", bucket.StartTime, bucket.EndTime, float64(result.InputCachedTokens))
				updateMetric(labels, "input_audio", bucket.StartTime, bucket.EndTime, float64(result.InputAudioTokens))
				updateMetric(labels, "output_audio", bucket.StartTime, bucket.EndTime, float64(result.OutputAudioTokens))

				logrus.Debugf("Processed result - Model: %s, Operation: %s, ProjectID: %s, UserID: %s, APIKeyID: %s, Batch: %s, BucketStart: %d, BucketEnd: %d, InputTokens: %d, OutputTokens: %d, InputCached: %d, InputAudio: %d, OutputAudio: %d, Requests: %d",
					deref(result.Model), endpoint.Name, deref(result.ProjectID), deref(result.UserID), deref(result.APIKeyID),
					string(result.Batch), bucket.StartTime, bucket.EndTime,
					result.InputTokens, result.OutputTokens, result.InputCachedTokens, result.InputAudioTokens, result.OutputAudioTokens, result.NumModelRequests)
			}
		}

		if !response.HasMore {
			break
		}
		nextPage = response.NextPage
	}

	logrus.Infof("Total records fetched from %s: %d", endpoint.Path, len(allResults))
	return nil
}

// ensureProjectName returns the name for already known projects and exports the name for new ones
func (e *Exporter) ensureProjectName(projectId string) string {
	if projectId == "" || projectId == "unknown" {
		return "unknown"
	}

	stateMu.RLock()
	if n, ok := projectNames[projectId]; ok && n != "" {
		stateMu.RUnlock()
		return n
	}
	stateMu.RUnlock()

	url := fmt.Sprintf("https://api.openai.com/v1/organization/projects/%s", projectId)
	logrus.Debugf("Fetching project name: %s", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "unknown"
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return "unknown"
	}
	defer func() { _ = resp.Body.Close() }()

	var obj Project
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil || obj.Name == "" {
		return "unknown"
	}
	logrus.Debugf("Received response: %+v", resp)

	stateMu.Lock()
	projectNames[projectId] = obj.Name
	stateMu.Unlock()
	return obj.Name
}

// ensureProjectName returns the name for already known API keys and exports the name for new ones
func (e *Exporter) ensureAPIKeyName(projectID, apiKeyID string) string {
	if apiKeyID == "" || apiKeyID == "unknown" {
		return "unknown"
	}

	stateMu.RLock()
	if n, ok := apiKeyNames[apiKeyID]; ok && n != "" {
		stateMu.RUnlock()
		return n
	}
	stateMu.RUnlock()

	var urls []string
	if projectID != "" && projectID != "unknown" {
		urls = append(urls,
			fmt.Sprintf("https://api.openai.com/v1/organization/projects/%s/api_keys/%s", projectID, apiKeyID))
	}
	urls = append(urls,
		fmt.Sprintf("https://api.openai.com/v1/organization/api_keys/%s", apiKeyID))

	for _, u := range urls {
		logrus.Debugf("Fetching api key name: %s", u)
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)

		if name, ok := func() (string, bool) {
			resp, err := e.client.Do(req)
			if err != nil {
				return "", false
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				_, _ = io.Copy(io.Discard, resp.Body) // дренируем
				return "", false
			}

			var obj APIKey
			if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
				return "", false
			}
			if obj.Name == "" {
				return "", false
			}
			return obj.Name, true
		}(); ok {
			stateMu.Lock()
			apiKeyNames[apiKeyID] = name
			stateMu.Unlock()
			return name
		}
	}

	return "unknown"
}

func deref(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}

// fetchCostData downloads information about the cost of projects
func (e *Exporter) fetchCostData(startTime, endTime int64) error {
	baseURL := "https://api.openai.com/v1/organization/costs"
	nextPage := ""

	for {
		url := fmt.Sprintf("%s?start_time=%d&end_time=%d&group_by=project_id",
			baseURL, startTime, endTime)
		if nextPage != "" {
			url += "&page=" + nextPage
		}

		logrus.Debugf("Fetching cost data: %s", url)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)

		resp, err := e.client.Do(req)
		if err != nil {
			return fmt.Errorf("error fetching cost data: %w", err)
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
		logrus.Debugf("Received response: %+v", resp)

		for _, bucket := range out.Data {
			if len(bucket.Results) > 0 {
				logrus.Debugf("Results %+v", bucket.Results)
			}
			date := time.Unix(bucket.StartTime, 0).UTC().Format("2006-01-02")
			for _, res := range bucket.Results {
				projectId := deref(res.ProjectID)
				lineName := "unknown"
				if res.LineItem != nil && *res.LineItem != "" {
					lineName = *res.LineItem
				}
				labels := prometheus.Labels{
					"date":            date,
					"project_id":      projectId,
					"project_name":    e.ensureProjectName(projectId),
					"line_item":       lineName,
					"organization_id": res.OrganizationID,
					"currency":        res.Amount.Currency,
				}
				dailyCostUSD.With(labels).Set(res.Amount.Value)
				logrus.Debugf("Processed result - Date: %s, ProjectID: %s, ProjectName: %s, LineItem: %s, OrganizationID: %s, Cost: %f, Currency: %s",
					date, projectId, e.ensureProjectName(projectId), lineName, res.OrganizationID, res.Amount.Value, res.Amount.Currency)
			}
		}

		if !out.HasMore {
			break
		}
		nextPage = out.NextPage
	}

	return nil
}

// collect performs a loop to gather data for the last time window (one minute).
// For each cycle, a time window is determined: from (current time - scrape.interval) to current time.
func (e *Exporter) collect() {
	stepSec := int64((*scrapeInterval) / time.Second)
	for {
		startTime := lastScrape
		endTime := lastScrape + stepSec

		logrus.Infof("Starting collection cycle: startTime=%d, endTime=%d", startTime, endTime)

		var wg sync.WaitGroup
		for _, endpoint := range usageEndpoints {
			wg.Add(1)
			go func(ep UsageEndpoint) {
				defer wg.Done()
				if err := e.fetchUsageData(ep, startTime, endTime); err != nil {
					logrus.WithError(err).Errorf("Error fetching data from %s", ep.Path)
				}
			}(endpoint)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.fetchCostData(startTime, endTime+60*60*24); err != nil {
				logrus.WithError(err).Warn("Error fetching cost data")
			}
		}()
		wg.Wait()
		lastScrape += stepSec
		time.Sleep(*scrapeInterval)
	}
}

// Main Function

func main() {
	flag.Parse()
	setupLogging()

	lastScrape = time.Now().Round(time.Minute).Add(-*scrapeInterval).Unix()
	exporter, err := NewExporter()
	if err != nil {
		logrus.Fatal(err)
	}

	go exporter.collect()

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("<html><head><title>OpenAI Exporter</title></head><body><h1>OpenAI Exporter</h1><p><a href='" + *metricsPath + "'>Metrics</a></p></body></html>"))
		if err != nil {
			logrus.WithError(err).Error("Failed to write response")
		}
	})

	logrus.Infof("Starting server on %s", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		logrus.Fatal(err)
	}
}
