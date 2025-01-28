package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Usage represents the OpenAI API usage data structure
type Usage struct {
	Data []struct {
		Timestamp       int64  `json:"aggregation_timestamp"`
		ContextTokens   int    `json:"n_context_tokens_total"`
		GeneratedTokens int    `json:"n_generated_tokens_total"`
		Model           string `json:"snapshot_id"`
	} `json:"data"`
}

// Prometheus metrics
var (
	// Counter for context tokens
	contextTokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openai_context_tokens_total",
			Help: "Total number of context tokens used",
		},
		[]string{"model", "organization_id", "project_id"},
	)

	// Counter for generated tokens
	generatedTokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "openai_generated_tokens_total",
			Help: "Total number of generated tokens used",
		},
		[]string{"model", "organization_id", "project_id"},
	)
)

func init() {
	// Register Prometheus metrics
	prometheus.MustRegister(contextTokensTotal)
	prometheus.MustRegister(generatedTokensTotal)
}

// Exporter handles metrics collection from OpenAI API
type Exporter struct {
	apiKey    string
	orgID     string
	projectID string
}

// NewExporter creates a new OpenAI metrics exporter
func NewExporter(apiKey, orgID, projectID string) *Exporter {
	return &Exporter{
		apiKey:    apiKey,
		orgID:     orgID,
		projectID: projectID,
	}
}

// fetchUsageData retrieves usage data from OpenAI API for a specific date
func (e *Exporter) fetchUsageData(date time.Time) (*Usage, error) {
	req := &http.Request{
		Method: "GET",
		URL: &url.URL{
			Scheme: "https",
			Host:   "api.openai.com",
			Path:   "/v1/usage",
			RawQuery: url.Values{
				"date": {date.Format("2006-01-02")},
			}.Encode(),
		},
		Header: http.Header{
			"Authorization":       {fmt.Sprintf("Bearer %s", e.apiKey)},
			"OpenAI-Organization": {e.orgID},
		},
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch usage data: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: %s - %s", resp.Status, string(body))
	}

	var usage Usage
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, fmt.Errorf("failed to parse usage data: %w", err)
	}

	return &usage, nil
}

// collect fetches and updates the metrics
func (e *Exporter) collect() {
	// Fetch today's usage data
	today := time.Now()
	usage, err := e.fetchUsageData(today)
	if err != nil {
		log.Printf("Error fetching usage data: %v", err)
		return
	}

	// Update Prometheus metrics
	for _, data := range usage.Data {
		model := data.Model
		if model == "" {
			model = "unknown"
		}

		contextTokensTotal.WithLabelValues(model, e.orgID, e.projectID).Add(float64(data.ContextTokens))
		generatedTokensTotal.WithLabelValues(model, e.orgID, e.projectID).Add(float64(data.GeneratedTokens))
	}
}

func main() {
	// Command line flags
	interval := flag.Duration("interval", 1*time.Minute, "Scrape interval (e.g. 1m, 30s)")
	flag.Parse()

	// Environment variables
	secretKey := os.Getenv("OPENAI_SECRET_KEY")
	orgID := os.Getenv("OPENAI_ORG_ID")
	projectID := os.Getenv("OPENAI_PROJECT_ID")

	if secretKey == "" {
		log.Fatal("OPENAI_SECRET_KEY environment variable is required")
	}

	if orgID == "" {
		log.Fatal("OPENAI_ORG_ID environment variable is required")
	}

	if projectID == "" {
		log.Fatal("OPENAI_PROJECT_ID environment variable is required")
	}

	// Initialize exporter
	exporter := NewExporter(secretKey, orgID, projectID)

	// Initial metrics collection
	exporter.collect()

	// Start periodic collection
	go func() {
		for {
			time.Sleep(*interval)
			exporter.collect()
		}
	}()

	// Setup HTTP server
	http.Handle("/metrics", promhttp.Handler())

	log.Printf("Starting OpenAI token exporter on :9100 with interval %v", interval)
	if err := http.ListenAndServe(":9100", nil); err != nil {
		log.Fatal(err)
	}
}
