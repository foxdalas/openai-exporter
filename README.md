# openai-exporter
OpenAI Prometheus Exporter

## Metrics Example
```
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="input",user_id=""} 1081
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="input_audio",user_id=""} 0
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="input_cached",user_id=""} 0
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="output",user_id=""} 1432
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",token_type="output_audio",user_id=""} 0
```
