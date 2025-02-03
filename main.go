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

// StringOrBool is a custom type that unmarshals a JSON value that may be either a string, a bool, or null.
// For a boolean value, it converts the value to its string representation ("true" or "false").
// For a null value, it returns "unknown".
type StringOrBool string

// UnmarshalJSON implements the json.Unmarshaler interface for StringOrBool.
func (s *StringOrBool) UnmarshalJSON(b []byte) error {
	// If the value is null, set s to "unknown".
	if string(b) == "null" {
		*s = "unknown"
		return nil
	}

	// If the JSON value is a string (starts with a quote), unmarshal it as a string.
	if b[0] == '"' {
		var tmp string
		if err := json.Unmarshal(b, &tmp); err != nil {
			return err
		}
		*s = StringOrBool(tmp)
		return nil
	}

	// Otherwise, try to unmarshal the value as a boolean.
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

	// Prometheus metric to count tokens with extended labels.
	// Extra labels include:
	// - batch: identifier from the API result (which may be a string or bool)
	// - token_type: distinguishes token counts ("input", "output", "input_cached", "input_audio", "output_audio")
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
	Object    string        `json:"object"`
	StartTime int64         `json:"start_time"`
	EndTime   int64         `json:"end_time"`
	Results   []UsageResult `json:"results"`
}

// UsageResult represents a single usage record.
// Extended to include additional token fields and the batch identifier.
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
// It updates the Prometheus metric with extended token information per token type.
func (e *Exporter) fetchUsageData(endpoint UsageEndpoint, startTime, endTime int64) error {
	baseURL := fmt.Sprintf("https://api.openai.com/v1/organization/usage/%s", endpoint.Path)
	nextPage := ""

	// allResults collects all usage records (for logging total count).
	allResults := []UsageResult{}

	for {
		// Build the request URL with query parameters.
		// Notice that we now group by batch as well.
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

				// Prepare common label values.
				labels := prometheus.Labels{
					"model":      deref(result.Model),
					"operation":  endpoint.Name,
					"project_id": deref(result.ProjectID),
					"user_id":    deref(result.UserID),
					"api_key_id": deref(result.APIKeyID),
					// Use our custom type's string value for the "batch" label.
					"batch": string(result.Batch),
				}

				// Update metric for input tokens.
				tokensTotal.With(mergeLabels(labels, "token_type", "input")).Add(float64(result.InputTokens))
				// Update metric for output tokens.
				tokensTotal.With(mergeLabels(labels, "token_type", "output")).Add(float64(result.OutputTokens))
				// Update metric for input cached tokens.
				tokensTotal.With(mergeLabels(labels, "token_type", "input_cached")).Add(float64(result.InputCachedTokens))
				// Update metric for input audio tokens.
				tokensTotal.With(mergeLabels(labels, "token_type", "input_audio")).Add(float64(result.InputAudioTokens))
				// Update metric for output audio tokens.
				tokensTotal.With(mergeLabels(labels, "token_type", "output_audio")).Add(float64(result.OutputAudioTokens))

				logrus.Debugf("Processed result - Model: %s, Operation: %s, ProjectID: %s, UserID: %s, APIKeyID: %s, Batch: %s, InputTokens: %d, OutputTokens: %d, InputCached: %d, InputAudio: %d, OutputAudio: %d, Requests: %d",
					deref(result.Model), endpoint.Name, deref(result.ProjectID), deref(result.UserID), deref(result.APIKeyID), string(result.Batch),
					result.InputTokens, result.OutputTokens, result.InputCachedTokens, result.InputAudioTokens, result.OutputAudioTokens, result.NumModelRequests)
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

// mergeLabels returns a new map merging base labels with an extra key-value pair.
func mergeLabels(base prometheus.Labels, key, value string) prometheus.Labels {
	newLabels := make(prometheus.Labels, len(base)+1)
	for k, v := range base {
		newLabels[k] = v
	}
	newLabels[key] = value
	return newLabels
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
