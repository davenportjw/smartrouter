import os
import logging
from typing import Optional
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
import httpx

# Setup logging
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("router-apikey-client")

app = FastAPI(
    title="Gemini Router API Key Client Example",
    description="A microservice demonstrating how to call the Gemini Smart Router using an API key."
)

# Load configurations from environment variables
ROUTER_URL = os.getenv("ROUTER_URL", "http://localhost:8080").rstrip("/")
ROUTER_API_KEY = os.getenv("ROUTER_API_KEY")

class GenerateRequest(BaseModel):
    prompt: str
    stream: bool = False

class GenerateResponse(BaseModel):
    text: str
    model_routed: Optional[str] = None
    client_tier: Optional[str] = None
    latency_ms: Optional[str] = None

@app.on_event("startup")
async def startup_event():
    if not ROUTER_API_KEY:
        logger.warning(
            "⚠️ ROUTER_API_KEY environment variable is not set! "
            "Requests to the Smart Router will fail until this is configured."
        )
    logger.info(f"🚀 Starting client. Target Gemini Smart Router URL: {ROUTER_URL}")

@app.get("/health")
async def health_check():
    return {"status": "healthy", "router_target": ROUTER_URL}

@app.post("/generate", response_model=GenerateResponse)
async def generate_content(payload: GenerateRequest):
    """
    Submits a prompt to the Gemini Smart Router using the cost-effective gemini-2.5-flash-lite model.
    """
    if not ROUTER_API_KEY:
        raise HTTPException(
            status_code=500,
            detail="ROUTER_API_KEY is not configured on this service."
        )

    # Target the cost-effective gemini-2.5-flash-lite model
    # The smart router will dynamically intercept, rate-limit, and proxy this request.
    url = f"{ROUTER_URL}/v1/models/gemini-2.5-flash-lite:generateContent"

    # Set headers. We pass the API Key using the 'x-goog-api-key' header.
    # Alternatively, the router also supports standard Authorization: Bearer tokens
    headers = {
        "Content-Type": "application/json",
        "x-goog-api-key": ROUTER_API_KEY
    }

    # Construct official Gemini API JSON body
    data = {
        "contents": [
            {
                "parts": [
                    {"text": payload.prompt}
                ]
            }
        ]
    }

    logger.info(f"Sending request to smart router: {url}")
    
    async with httpx.AsyncClient() as client:
        try:
            response = await client.post(
                url,
                json=data,
                headers=headers,
                timeout=30.0
            )
            
            # If the router returned rate limits (429) or auth errors (401), forward details clearly
            if response.status_code != 200:
                logger.error(f"Router error ({response.status_code}): {response.text}")
                try:
                    error_payload = response.json()
                    error_msg = error_payload.get("error", {}).get("message", "Unknown upstream router error")
                except Exception:
                    error_msg = response.text or "Failed to connect"
                
                raise HTTPException(
                    status_code=response.status_code,
                    detail=f"Smart Router Error: {error_msg}"
                )

            response_json = response.json()
            
            # Extract the generated text from official Gemini schema
            try:
                candidates = response_json.get("candidates", [])
                if not candidates:
                    raise KeyError()
                parts = candidates[0].get("content", {}).get("parts", [])
                generated_text = parts[0].get("text", "")
            except (IndexError, KeyError, TypeError):
                logger.error(f"Unexpected response format from Gemini API: {response_json}")
                raise HTTPException(
                    status_code=502,
                    detail="Bad Gateway: Upstream Gemini API returned an unexpected schema."
                )

            # Parse headers returned by the smart router for auditing/logging
            model_routed = response.headers.get("X-Routed-Model") or "gemini-2.5-flash-lite"
            client_tier = response.headers.get("X-Client-Tier")
            latency = response.headers.get("X-Response-Time") # Set by GCP load balancers or router

            return GenerateResponse(
                text=generated_text,
                model_routed=model_routed,
                client_tier=client_tier,
                latency_ms=latency
            )

        except httpx.RequestError as exc:
            logger.error(f"HTTP Request failed: {exc}")
            raise HTTPException(
                status_code=503,
                detail=f"Failed to reach Gemini Smart Router at {ROUTER_URL}: {exc}"
            )
