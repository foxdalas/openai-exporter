[![Docker Pulls](https://img.shields.io/docker/pulls/foxdalas/openai-exporter?style=flat-square)](https://hub.docker.com/r/foxdalas/openai-exporter) [![GitHub Release](https://img.shields.io/github/v/release/foxdalas/openai-exporter?style=flat-square)](https://github.com/foxdalas/openai-exporter/releases)



# OpenAI Exporter

This is an OpenAI Prometheus Exporter designed to fetch and expose usage data metrics from OpenAI's APIs. The exporter provides metrics related to the utilization of various OpenAI services, such as completions, embeddings, and moderations, suitable for monitoring and analytics purposes.

## Features

- Fetches usage data from OpenAI APIs.
- Exposes metrics in Prometheus format.
- Configurable metrics collection intervals.
- Handles multiple types of data endpoints.

## Prerequisites

- Go 1.x or higher
- Access to OpenAI API and organization IDs

## Configuration

Before running the exporter, ensure the following environment variables are set:
- `OPENAI_SECRET_KEY`: Your OpenAI API secret key.
- `OPENAI_ORG_ID`: Your organization ID with OpenAI.

## Installation

```bash
go build -o openai-exporter
```

## Usage
```
./openai-exporter
```

### Docker
```
docker run -d -p 9185:9185 -e OPENAI_SECRET_KEY=your_secret_key -e OPENAI_ORG_ID=your_org_id foxdalas/openai-exporter:v0.0.11
```

Use the following flags to customize the behavior:

* `-web.listen-address`: Set the listen address for the web interface and telemetry (default: :9185).
* `-web.telemetry-path`: Set the path under which to expose metrics (default: /metrics).
* `-scrape.interval`: Set the interval for API calls and data collection (default: 1m).
* `-log.level`: Set the log verbosity (default: info).

## Metrics Exampes
Metrics are exposed in the Prometheus format, which can be queried via HTTP GET requests on the telemetry path. Here's an example of the kind of metrics you might see:
```
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="input",user_id=""} 1081
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="input_audio",user_id=""} 0
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="input_cached",user_id=""} 0
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="output",user_id=""} 1432
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="output_audio",user_id=""} 0
```

## Contributing
Contributions to the OpenAI Exporter are welcome. Please ensure that your commits meet the following criteria:
* Code must be well-documented.
* Commits must be clear and concise.
* All changes must be tested thoroughly.

## License
This project is licensed under the MIT License - see the [LICENSE](https://github.com/foxdalas/openai-exporter/blob/master/LICENSE) file for details.
