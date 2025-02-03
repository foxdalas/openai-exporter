package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// UsageEndpoint represents an API usage endpoint.
type UsageEndpoint struct {
	Path string // API endpoint path (e.g. "completions")
	Name string // Name of the operation (e.g. "completions")
}

var (
	// CLI flags for configuring the exporter.
	listenAddress  = flag.String("web.listen-address", ":9185", "Address to listen on for web interface and telemetry")
	metricsPath    = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics")
	scrapeInterval = flag.Duration("scrape.interval", 1*time.Minute, "Interval for API calls and data window")
	queryOffset    = flag.Duration("query.offset", 60*time.Minute, "Offset window for API queries")
	logLevel       = flag.String("log.level", "info", "Log level")

	// Available endpoints for which usage data will be fetched.
	usageEndpoints = []UsageEndpoint{
		{Path: "completions", Name: "completions"},
		{Path: "embeddings", Name: "embeddings"},
		{Path: "moderations", Name: "moderations"},
		{Path: "images", Name: "images"},
		{Path: "audio_speeches", Name: "audio_speeches"},
		{Path: "audio_transcriptions", Name: "audio_transcriptions"},
		{Path: "vector_stores", Name: "vector_stores"},
	}

	// Prometheus metric to count the total tokens used.
	tokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openai_api_tokens_total",
			Help: "Total number of tokens used per model, operation, project, user, and API key",
		},
		[]string{"model", "operation", "project_id", "user_id", "api_key_id", "type"},
	)
)

func init() {
	flag.Parse()

	// Parse and set the log level.
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse log level")
	}
	logrus.SetLevel(level)
	logrus.Infof("Log level set to %s", level)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// Register the Prometheus metric.
	prometheus.MustRegister(tokensTotal)
	logrus.Info("Metrics registered successfully")
}

// Exporter holds the HTTP client and credentials for fetching usage data.
type Exporter struct {
	client *http.Client
	apiKey string
	orgID  string
}

// APIResponse is the structure of the response from the OpenAI API.
type APIResponse struct {
	Object   string   `json:"object"`
	Data     []Bucket `json:"data"`
	HasMore  bool     `json:"has_more"`
	NextPage string   `json:"next_page"`
}

// Bucket represents a time bucket in the API response.
type Bucket struct {
	StartTime int64         `json:"start_time"`
	EndTime   int64         `json:"end_time"`
	Results   []UsageResult `json:"results"`
}

// UsageResult represents a single usage record.
type UsageResult struct {
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	NumModelRequests int64   `json:"num_model_requests"`
	ProjectID        *string `json:"project_id"`
	UserID           *string `json:"user_id"`
	APIKeyID         *string `json:"api_key_id"`
	Model            *string `json:"model"`
}

// NewExporter creates a new Exporter instance using credentials from environment variables.
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

// fetchUsageData fetches usage data for a given endpoint and time window.
// It also updates the Prometheus metric with the total tokens (input + output).
func (e *Exporter) fetchUsageData(endpoint UsageEndpoint, startTime, endTime int64) error {
	baseURL := fmt.Sprintf("https://api.openai.com/v1/organization/usage/%s", endpoint.Path)
	nextPage := ""

	// allResults collects all usage records (used here only for logging the total count).
	allResults := []UsageResult{}

	for {
		// Build the request URL with query parameters.
		url := fmt.Sprintf("%s?start_time=%d&end_time=%d&bucket_width=1m&limit=1440&group_by=project_id,user_id,api_key_id,model",
			baseURL, startTime, endTime)
		if nextPage != "" {
			url += "&page=" + nextPage
		}

		logrus.Debugf("Fetching usage data: %s", url)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		// Set the authorization header.
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

		// Process each bucket and update metrics for every usage record.
		for _, bucket := range response.Data {
			for _, result := range bucket.Results {
				allResults = append(allResults, result)

				// Update the Prometheus counter metric with the total tokens used.
				// Here, we sum the input and output tokens.
				tokensTotal.With(prometheus.Labels{
					"model":      deref(result.Model),
					"operation":  endpoint.Name, // Label by operation (endpoint name)
					"project_id": deref(result.ProjectID),
					"user_id":    deref(result.UserID),
					"api_key_id": deref(result.APIKeyID),
					"type":       "total", // Token type label (can be extended if needed)
				}).Add(float64(result.InputTokens + result.OutputTokens))

				logrus.Debugf("ProjectID: %s, UserID: %s, APIKeyID: %s, Model: %s, InputTokens: %d, OutputTokens: %d, Requests: %d",
					deref(result.ProjectID), deref(result.UserID), deref(result.APIKeyID), deref(result.Model),
					result.InputTokens, result.OutputTokens, result.NumModelRequests)
			}
		}

		// If there are no more pages, exit the loop.
		if !response.HasMore {
			break
		}
		nextPage = response.NextPage
	}

	logrus.Infof("Total records fetched from %s: %d", endpoint.Path, len(allResults))
	return nil
}

// deref returns the value of a string pointer or "unknown" if it is nil.
func deref(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}

// collect starts the periodic collection of usage data from all endpoints.
func (e *Exporter) collect() {
	for {
		logrus.Info("Starting collection cycle")

		startTime := time.Now().Add(-*queryOffset).Unix()
		endTime := time.Now().Unix()

		var wg sync.WaitGroup
		// Fetch data concurrently for each endpoint.
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
		// Wait for the configured scrape interval before starting the next cycle.
		time.Sleep(*scrapeInterval)
	}
}

// main initializes the exporter and starts the HTTP server to expose metrics.
func main() {
	exporter, err := NewExporter()
	if err != nil {
		logrus.Fatal(err)
	}

	// Start the usage data collection in a separate goroutine.
	go exporter.collect()

	// Set up HTTP handlers for Prometheus metrics and a simple root page.
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
