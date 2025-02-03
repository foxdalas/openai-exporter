package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// -------------------------
// Custom Type for Batch Field
// -------------------------

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

// -------------------------
// Global Variables and State
// -------------------------

var (
	stateMu    sync.Mutex
	usageState = make(map[string]float64)
	// lastScrapeTime holds the timestamp (Unix seconds) of the previous scrape.
	// On the first scrape (when lastScrapeTime==0) мы только сохраняем базовые значения.
	lastScrapeTime int64 = 0
)

// -------------------------
// Prometheus Metric and CLI Flags
// -------------------------

type UsageEndpoint struct {
	Path string // API endpoint path (e.g. "completions")
	Name string // Name of the operation (e.g. "completions")
}

var (
	listenAddress  = flag.String("web.listen-address", ":9185", "Address to listen on for web interface and telemetry")
	metricsPath    = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics")
	scrapeInterval = flag.Duration("scrape.interval", 1*time.Minute, "Interval for API calls and data window")
	queryOffset    = flag.Duration("query.offset", 60*time.Minute, "Offset window for API queries")
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
		[]string{"model", "operation", "project_id", "user_id", "api_key_id", "batch", "token_type"},
	)
)

func init() {
	flag.Parse()

	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse log level")
	}
	logrus.SetLevel(level)
	logrus.Infof("Log level set to %s", level)
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	prometheus.MustRegister(tokensTotal)
	logrus.Info("Metrics registered successfully")
}

// -------------------------
// Exporter and API Structures
// -------------------------

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

// -------------------------
// Helper Functions for State and Metrics
// -------------------------

func mergeLabels(base prometheus.Labels, key, value string) prometheus.Labels {
	newLabels := make(prometheus.Labels, len(base)+1)
	for k, v := range base {
		newLabels[k] = v
	}
	newLabels[key] = value
	return newLabels
}

// updateMetric обновляет метрику для данного токен-тайпа, используя следующую логику:
//  1. Если это первый скрап (lastScrapeTime == 0), то базовое значение просто сохраняется, без увеличения счётчика.
//  2. Если бакет является новым (bucketStart > lastScrapeTime), то к метрике прибавляется полное значение newValue.
//  3. Если бакет уже виделся, но он открыт (bucket.EndTime > текущего времени),
//     то вычисляется дельта между новым значением и сохранённым, и дельта прибавляется к метрике.
func updateMetric(labels prometheus.Labels, tokenType string, bucketStart, bucketEnd int64, newValue float64) {
	// Композитный ключ формируется как:
	// operation|bucketStart|project_id|user_id|api_key_id|model|batch|token_type
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
	stateMu.Lock()
	defer stateMu.Unlock()

	// Если это первый скрап, сохраняем базовое значение и не обновляем метрику.
	if lastScrapeTime == 0 {
		usageState[compositeKey] = newValue
		return
	}

	// Если бакет новый (начался после предыдущего скрапа), считаем всё его значение как новый.
	if bucketStart > lastScrapeTime {
		tokensTotal.With(mergeLabels(labels, "token_type", tokenType)).Add(newValue)
		usageState[compositeKey] = newValue
		return
	}

	// Если бакет уже виделся, и он ещё открыт (возможно, его значение растёт),
	// то вычисляем прирост.
	if bucketEnd > now {
		prev, exists := usageState[compositeKey]
		if !exists {
			// На всякий случай – если ключ отсутствует, считаем как новый бакет.
			tokensTotal.With(mergeLabels(labels, "token_type", tokenType)).Add(newValue)
			usageState[compositeKey] = newValue
		} else {
			delta := newValue - prev
			if delta < 0 {
				logrus.Warnf("Detected decrease for key %s: prev=%f, new=%f", compositeKey, prev, newValue)
				delta = 0
			}
			tokensTotal.With(mergeLabels(labels, "token_type", tokenType)).Add(delta)
			usageState[compositeKey] = newValue
		}
	}
	// Если бакет закрыт и уже виделся, ничего не делаем.
}

// -------------------------
// Data Collection
// -------------------------

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
		defer resp.Body.Close()

		var response APIResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return fmt.Errorf("error decoding response: %w", err)
		}

		for _, bucket := range response.Data {
			for _, result := range bucket.Results {
				allResults = append(allResults, result)

				labels := prometheus.Labels{
					"model":      deref(result.Model),
					"operation":  endpoint.Name,
					"project_id": deref(result.ProjectID),
					"user_id":    deref(result.UserID),
					"api_key_id": deref(result.APIKeyID),
					"batch":      string(result.Batch),
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

func deref(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}

func (e *Exporter) collect() {
	for {
		currentScrapeTime := time.Now().Unix()
		logrus.Info("Starting collection cycle")

		startTime := time.Now().Add(-*queryOffset).Unix()
		endTime := currentScrapeTime

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
		wg.Wait()

		// После скрапа обновляем lastScrapeTime,
		// чтобы в следующий раз учитывать только бакеты, начинающиеся после этого времени.
		stateMu.Lock()
		if lastScrapeTime == 0 {
			// Первый скрап – запоминаем текущее время и не обновляем метрику.
			lastScrapeTime = currentScrapeTime
		} else {
			lastScrapeTime = currentScrapeTime
		}
		stateMu.Unlock()

		time.Sleep(*scrapeInterval)
	}
}

// -------------------------
// Main Function
// -------------------------

func main() {
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
