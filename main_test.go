package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerOnce ensures registerMetrics() runs at most once across the test
// binary, since prometheus.MustRegister panics on duplicate registration.
var registerOnce sync.Once

func ensureMetricsRegistered() {
	registerOnce.Do(registerMetrics)
}

func TestStringOrBool_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected StringOrBool
		wantErr  bool
	}{
		{
			name:     "null value",
			input:    `null`,
			expected: "unknown",
			wantErr:  false,
		},
		{
			name:     "string value",
			input:    `"batch_123"`,
			expected: "batch_123",
			wantErr:  false,
		},
		{
			name:     "bool true",
			input:    `true`,
			expected: "true",
			wantErr:  false,
		},
		{
			name:     "bool false",
			input:    `false`,
			expected: "false",
			wantErr:  false,
		},
		{
			name:     "invalid type",
			input:    `123`,
			expected: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s StringOrBool
			err := json.Unmarshal([]byte(tt.input), &s)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, s)
			}
		})
	}
}

func TestFloatOrString_UnmarshalJSON_String(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected FloatOrString
		wantErr  bool
	}{
		{
			name:     "string value",
			input:    "123.45",
			expected: 123.45,
			wantErr:  false,
		},
		{
			name:     "string value many zeros",
			input:    "0.1739150000000000000000000000",
			expected: 0.173915,
			wantErr:  false,
		},
		{
			name:     "int as string",
			input:    "73",
			expected: 73.0,
			wantErr:  false,
		},
		{
			name:     "invalid string value",
			input:    "foo",
			expected: 0,
			wantErr:  true,
		},
		{
			name:     "invalid type",
			input:    `[]`,
			expected: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FloatOrString
			err := json.Unmarshal([]byte(tt.input), &f)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, f)
			}
		})
	}
}

func TestFloatOrString_UnmarshalJSON_Float(t *testing.T) {
	tests := []struct {
		name     string
		input    float64
		expected FloatOrString
		wantErr  bool
	}{
		{
			name:     "2 decimals",
			input:    123.45,
			expected: 123.45,
			wantErr:  false,
		},
		{
			name:     "6 decimals",
			input:    0.173915,
			expected: 0.173915,
			wantErr:  false,
		},
		{
			name:     "int",
			input:    73,
			expected: 73.0,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FloatOrString
			b, errMarshal := json.Marshal(tt.input)
			assert.NoError(t, errMarshal)
			err := json.Unmarshal(b, &f)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, f)
			}
		})
	}
}

func TestMoney_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected Money
		wantErr  bool
	}{
		{
			name:  "string value",
			input: `{"currency": "usd", "value": "0.1739150000000000000000000000"}`,
			expected: Money{
				Value:    0.173915,
				Currency: "usd",
			},
			wantErr: false,
		},
		{
			name:  "float64 value",
			input: `{"currency": "usd", "value": 0.173915}`,
			expected: Money{
				Value:    0.173915,
				Currency: "usd",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m Money
			err := json.Unmarshal([]byte(tt.input), &m)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, m)
			}
		})
	}
}

func TestDeref(t *testing.T) {
	tests := []struct {
		name     string
		input    *string
		expected string
	}{
		{
			name:     "nil pointer",
			input:    nil,
			expected: "unknown",
		},
		{
			name:     "valid string",
			input:    strPtr("test-value"),
			expected: "test-value",
		},
		{
			name:     "empty string",
			input:    strPtr(""),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deref(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMergeLabels(t *testing.T) {
	base := prometheus.Labels{
		"model":     "gpt-4",
		"operation": "completions",
	}

	result := mergeLabels(base, "token_type", "input")

	assert.Len(t, result, 3)
	assert.Equal(t, "gpt-4", result["model"])
	assert.Equal(t, "completions", result["operation"])
	assert.Equal(t, "input", result["token_type"])

	assert.Len(t, base, 2)
}

func TestUpdateMetric(t *testing.T) {
	ensureMetricsRegistered()
	usageState = make(map[string]float64)

	labels := prometheus.Labels{
		"model":        "gpt-4",
		"operation":    "completions",
		"project_id":   "proj-123",
		"project_name": "test-project",
		"user_id":      "user-456",
		"api_key_id":   "key-789",
		"api_key_name": "key-name",
		"batch":        "false",
	}

	now := time.Now().Unix()
	bucketStart := now - 120
	bucketEnd := now - 60

	t.Run("processes completed bucket", func(t *testing.T) {
		usageState = make(map[string]float64)
		updateMetric(labels, "input", bucketStart, bucketEnd, 100.0)
		assert.Len(t, usageState, 1)
	})

	t.Run("skips incomplete bucket", func(t *testing.T) {
		usageState = make(map[string]float64)
		futureEnd := now + 60
		updateMetric(labels, "input", bucketStart, futureEnd, 100.0)
		assert.Len(t, usageState, 0)
	})

	t.Run("skips already processed bucket", func(t *testing.T) {
		usageState = make(map[string]float64)
		updateMetric(labels, "input", bucketStart, bucketEnd, 100.0)
		initialLen := len(usageState)
		updateMetric(labels, "input", bucketStart, bucketEnd, 200.0)
		assert.Len(t, usageState, initialLen)
	})
}

func TestNewExporter(t *testing.T) {
	t.Run("missing OPENAI_SECRET_KEY", func(t *testing.T) {
		t.Setenv("OPENAI_SECRET_KEY", "")
		t.Setenv("OPENAI_ORG_ID", "org-123")

		_, err := NewExporter()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "OPENAI_SECRET_KEY")
	})

	t.Run("OPENAI_ORG_ID is optional", func(t *testing.T) {
		t.Setenv("OPENAI_SECRET_KEY", "sk-test")
		t.Setenv("OPENAI_ORG_ID", "")

		exporter, err := NewExporter()
		require.NoError(t, err)
		assert.Equal(t, "", exporter.orgHeader)
	})

	t.Run("valid environment", func(t *testing.T) {
		t.Setenv("OPENAI_SECRET_KEY", "sk-test")
		t.Setenv("OPENAI_ORG_ID", "org-123")

		exporter, err := NewExporter()
		require.NoError(t, err)
		assert.NotNil(t, exporter)
		assert.NotNil(t, exporter.client)
		assert.Equal(t, "sk-test", exporter.apiKey)
		assert.Equal(t, "org-123", exporter.orgHeader)
	})
}

func TestEnsureProjectName(t *testing.T) {
	projectNames = make(map[string]string)
	ctx := context.Background()

	t.Run("empty project id", func(t *testing.T) {
		e := &Exporter{apiKey: "test"}
		result := e.ensureProjectName(ctx, "")
		assert.Equal(t, "unknown", result)
	})

	t.Run("unknown project id", func(t *testing.T) {
		e := &Exporter{apiKey: "test"}
		result := e.ensureProjectName(ctx, "unknown")
		assert.Equal(t, "unknown", result)
	})

	t.Run("cached project name", func(t *testing.T) {
		projectNames = make(map[string]string)
		projectNames["proj-123"] = "cached-project"

		e := &Exporter{apiKey: "test"}
		result := e.ensureProjectName(ctx, "proj-123")
		assert.Equal(t, "cached-project", result)
	})

	t.Run("fetch project name from API (parsing only)", func(t *testing.T) {
		projectNames = make(map[string]string)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Project{Name: "fetched-project"})
		}))
		defer server.Close()

		client := &http.Client{}

		req, _ := http.NewRequest("GET", server.URL, nil)
		req.Header.Set("Authorization", "Bearer test-key")

		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		var proj Project
		err = json.NewDecoder(resp.Body).Decode(&proj)
		require.NoError(t, err)

		assert.Equal(t, "fetched-project", proj.Name)
	})

	t.Run("API error returns unknown", func(t *testing.T) {
		projectNames = make(map[string]string)

		e := &Exporter{
			client: &http.Client{Timeout: 1 * time.Millisecond},
			apiKey: "test-key",
		}

		result := e.ensureProjectName(ctx, "proj-timeout")
		assert.Equal(t, "unknown", result)
	})
}

func TestEnsureAPIKeyName(t *testing.T) {
	apiKeyNames = make(map[string]string)
	ctx := context.Background()

	t.Run("empty api key id", func(t *testing.T) {
		e := &Exporter{apiKey: "test"}
		result := e.ensureAPIKeyName(ctx, "proj-123", "")
		assert.Equal(t, "unknown", result)
	})

	t.Run("unknown api key id", func(t *testing.T) {
		e := &Exporter{apiKey: "test"}
		result := e.ensureAPIKeyName(ctx, "proj-123", "unknown")
		assert.Equal(t, "unknown", result)
	})

	t.Run("cached api key name", func(t *testing.T) {
		apiKeyNames = make(map[string]string)
		apiKeyNames["key-123"] = "cached-key"

		e := &Exporter{apiKey: "test"}
		result := e.ensureAPIKeyName(ctx, "proj-123", "key-123")
		assert.Equal(t, "cached-key", result)
	})

	t.Run("fetch api key name from API (parsing only)", func(t *testing.T) {
		apiKeyNames = make(map[string]string)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(APIKey{Name: "fetched-key"})
		}))
		defer server.Close()

		client := &http.Client{}
		req, _ := http.NewRequest("GET", server.URL, nil)
		req.Header.Set("Authorization", "Bearer test-key")

		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		var k APIKey
		err = json.NewDecoder(resp.Body).Decode(&k)
		require.NoError(t, err)

		assert.Equal(t, "fetched-key", k.Name)
	})

	t.Run("API error returns unknown", func(t *testing.T) {
		apiKeyNames = make(map[string]string)

		e := &Exporter{
			client: &http.Client{Timeout: 1 * time.Millisecond},
			apiKey: "test-key",
		}

		result := e.ensureAPIKeyName(ctx, "proj-any", "key-timeout")
		assert.Equal(t, "unknown", result)
	})
}

func TestFetchUsageData_ErrorCases(t *testing.T) {
	ensureMetricsRegistered()
	ctx := context.Background()
	t.Run("HTTP request error", func(t *testing.T) {
		e := &Exporter{
			client: &http.Client{Timeout: 1 * time.Millisecond},
			apiKey: "test-key",
		}

		endpoint := UsageEndpoint{Path: "completions", Name: "completions"}
		err := e.fetchUsageData(ctx, endpoint, 1000, 2000)
		assert.Error(t, err)
	})

	t.Run("invalid JSON response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("invalid json"))
		}))
		defer server.Close()

		client := &http.Client{
			Transport: &http.Transport{
				Proxy: func(req *http.Request) (*url.URL, error) {
					return url.Parse(server.URL)
				},
			},
		}

		e := &Exporter{
			client: client,
			apiKey: "test-key",
		}

		endpoint := UsageEndpoint{Path: "completions", Name: "completions"}
		err := e.fetchUsageData(ctx, endpoint, 1000, 2000)
		assert.Error(t, err)
	})
}

func TestFetchCostData_ErrorCases(t *testing.T) {
	ensureMetricsRegistered()
	ctx := context.Background()
	t.Run("HTTP request error", func(t *testing.T) {
		e := &Exporter{
			client: &http.Client{Timeout: 1 * time.Millisecond},
			apiKey: "test-key",
		}

		err := e.fetchCostData(ctx, 1000, 2000)
		assert.Error(t, err)
	})

	t.Run("invalid JSON response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("invalid json"))
		}))
		defer server.Close()

		client := &http.Client{
			Transport: &http.Transport{
				Proxy: func(req *http.Request) (*url.URL, error) {
					return url.Parse(server.URL)
				},
			},
		}

		e := &Exporter{
			client: client,
			apiKey: "test-key",
		}

		err := e.fetchCostData(ctx, 1000, 2000)
		assert.Error(t, err)
	})
}

func strPtr(s string) *string {
	return &s
}
