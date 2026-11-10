# 🔌 Client Integration & Multi-SDK Guide

This guide explains the request formats, authentication headers, and copy-ready SDK configurations for integrating with the Smart Router using **OpenAI Spec SDKs** (`openai`, LangChain, LiteLLM), **Google GenAI SDKs** (`google-genai`), and raw HTTP (`curl`).

---

## 🧠 Authentication & Hierarchy Context

To integrate with the Smart Router, your credentials operate within a three-tier data hierarchy:

- **Client Organization**: The top-level subscriber or department account governing the subscription tier (`premium`, `standard`, `free`).
- **Logical Application (App)**: A functional project or service under the Client Organization (e.g., `customer-support-bot`, `internal-hr-portal`). **All RPM and TPM rate limits, along with request queueing priorities and routing rules, are governed at the Application level.**
- **Credentials (API Key or Google IAM)**: Bind directly to a single **Application**.

---

## 🔐 Authentication Methods

The proxy supports two authentication mechanisms across both **OpenAI-compatible (`/v1/*`)** and **Google Gemini Native (`/v1/models/*:generateContent`)** endpoints:

1. **Static Router API Key**:
   - **OpenAI Spec Header**: `Authorization: Bearer gr_key_your_custom_api_key`
   - **Google Gemini Header**: `x-goog-api-key: gr_key_your_custom_api_key`
   - **Query Parameter**: `?key=gr_key_your_custom_api_key`
2. **Zero-Key IAM Service Account OIDC**:
   - **Header**: `Authorization: Bearer <GCP_OIDC_IDENTITY_TOKEN>` (automatically verified against the application's bound `service_account` email).

---

## 🌐 1. OpenAI Spec Compatibility (`/v1/chat/completions` & `/v1/models`)

The Smart Router provides native support for the **OpenAI Chat Completions & Models Specification**, enabling any OpenAI-compatible client, agent framework, or IDE extension to route through the 4-Stage Dynamic Complexity Engine (`gemini-dynamic`), conditional header rules, rate limiters, and priority queues.

### Supported OpenAI Endpoints
| Method & Path | Description |
| :--- | :--- |
| `POST /v1/chat/completions` | Standard OpenAI Chat Completions (non-streaming & SSE `stream: true`, system/developer/user/assistant/tool messages, function `tools`, `response_format`, `temperature`, `max_tokens`). |
| `POST /v1beta/openai/chat/completions` | Vertex/Gemini OpenAI compatibility alias for Chat Completions. |
| `GET /v1/models` | Lists `gemini-dynamic` and all active models (`gemini-3.8-flash`, `gemini-3.5-flash-lite`, `gemini-3.1-pro`, `gemini-2.5-flash`, `gemma-4-*`) in OpenAI `{"object": "list", "data": [...]}` format. |
| `GET /v1/models/{model_id}` | Retrieves metadata for a single model in OpenAI `{"object": "model", ...}` format. |

### Python (`openai` Official SDK)

```python
from openai import OpenAI

client = OpenAI(
    api_key="gr_key_your_custom_api_key",
    base_url="https://gemini-smart-router-txgsracloq-uc.a.run.app/v1",
)

# 1. Dynamic Complexity Routing (Stage 0-3 Jev / Fastino / Contrastive-LM / LLM)
response = client.chat.completions.create(
    model="gemini-dynamic",
    messages=[
        {"role": "system", "content": "You are a concise cloud architect."},
        {"role": "user", "content": "Explain consensus in distributed systems in two sentences."},
    ],
    extra_headers={
        "X-Session-ID": "sess-prod-01",
        "X-Agent-Task": "architecture-summary",
    },
)
print("Model Used:", response.model)
print("Content:", response.choices[0].message.content)

# 2. Streaming Chat Completions (SSE)
stream = client.chat.completions.create(
    model="gemini-3.8-flash",
    messages=[{"role": "user", "content": "Count from 1 to 3."}],
    stream=True,
)
for chunk in stream:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

### TypeScript / Node.js (`openai` Official SDK)

```typescript
import OpenAI from "openai";

const client = new OpenAI({
  apiKey: process.env.SMART_ROUTER_API_KEY,
  baseURL: "https://gemini-smart-router-txgsracloq-uc.a.run.app/v1",
});

const completion = await client.chat.completions.create({
  model: "gemini-dynamic",
  messages: [
    { role: "system", content: "You are a helpful coding assistant." },
    { role: "user", content: "Write a Go HTTP health check handler." },
  ],
});

console.log(`Routed Model: ${completion.model}`);
console.log(completion.choices[0]?.message?.content);
```

### LangChain (`langchain-openai`)

```python
from langchain_openai import ChatOpenAI
from langchain_core.messages import HumanMessage, SystemMessage

llm = ChatOpenAI(
    model="gemini-dynamic",
    api_key="gr_key_your_custom_api_key",
    base_url="https://gemini-smart-router-txgsracloq-uc.a.run.app/v1",
    temperature=0.2,
)

resp = llm.invoke([
    SystemMessage(content="Answer concisely."),
    HumanMessage(content="What is the difference between a mutex and a semaphore?"),
])
print(resp.content)
```

### LiteLLM (`litellm`)

```python
import litellm

response = litellm.completion(
    model="openai/gemini-dynamic",
    api_base="https://gemini-smart-router-txgsracloq-uc.a.run.app/v1",
    api_key="gr_key_your_custom_api_key",
    messages=[{"role": "user", "content": "Hello from LiteLLM!"}],
)
print(response.choices[0].message.content)
```

### OpenAI Spec cURL (`/v1/chat/completions` & `/v1/models`)

```bash
# Chat Completions with Bearer API Key
curl -X POST "https://gemini-smart-router-txgsracloq-uc.a.run.app/v1/chat/completions" \
  -H "Authorization: Bearer gr_key_your_custom_api_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-dynamic",
    "messages": [
      {"role": "system", "content": "Be brief."},
      {"role": "user", "content": "Say hello."}
    ]
  }'

# List Available Models
curl -X GET "https://gemini-smart-router-txgsracloq-uc.a.run.app/v1/models" \
  -H "Authorization: Bearer gr_key_your_custom_api_key"
```

---

## ✨ 2. Google GenAI SDK (`google-genai`) & Native Gemini API

### Python (`google-genai` SDK)

```python
from google import genai
from google.genai import types

client = genai.Client(
    api_key="gr_key_your_custom_api_key",
    http_options={
        "api_endpoint": "https://gemini-smart-router-txgsracloq-uc.a.run.app"
    },
)

response = client.models.generate_content(
    model="gemini-dynamic",
    contents="Summarize the benefits of dynamic complexity routing.",
    config=types.GenerateContentConfig(
        http_headers={"X-Session-ID": "sess-101"}
    ),
)
print(response.text)
```

### Gemini Native cURL (`:generateContent`)

```bash
curl -X POST "https://gemini-smart-router-txgsracloq-uc.a.run.app/v1beta/models/gemini-3.8-flash:generateContent" \
  -H "x-goog-api-key: gr_key_your_custom_api_key" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{
      "parts": [{"text": "Summarize this text."}]
    }]
  }'
```

---

## 🛡️ 3. Zero-Key IAM Service Account OIDC

OIDC tokens eliminate the need to distribute, rotate, or store static API secrets for workloads running inside Google Cloud.

```bash
# 1. Retrieve OIDC Identity Token from local GCP Metadata Server
OIDC_TOKEN=$(curl -H "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=https://gemini-smart-router-txgsracloq-uc.a.run.app")

# 2. Call either the OpenAI endpoint or Gemini Native endpoint with the OIDC token
curl -X POST "https://gemini-smart-router-txgsracloq-uc.a.run.app/v1/chat/completions" \
  -H "Authorization: Bearer $OIDC_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-dynamic",
    "messages": [{"role": "user", "content": "Explain Zero-Key auth."}]
  }'
```
