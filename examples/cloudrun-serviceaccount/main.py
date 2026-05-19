import os
import logging
from typing import Optional
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
import httpx
import google.auth
import google.auth.transport.requests
import google.oauth2.id_token

# Setup logging
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("router-serviceaccount-client")

app = FastAPI(
    title="Gemini Router Service Account Client Example",
    description="A microservice demonstrating zero-key IAM authentication with the Gemini Smart Router."
)

# Load configurations from environment variables
ROUTER_URL = os.getenv("ROUTER_URL", "http://localhost:8080").rstrip("/")

class GenerateRequest(BaseModel):
    prompt: str

class GenerateResponse(BaseModel):
    text: str
    model_routed: Optional[str] = None
    client_tier: Optional[str] = None
    latency_ms: Optional[str] = None

# Pre-fetch local credentials context
try:
    credentials, project = google.auth.default()
    logger.info(f"Detected Google Default Credentials. Current Project: {project}")
except Exception as e:
    logger.warning(f"Could not detect Google Default Credentials: {e}. Local testing will require credentials.")

def get_oidc_token(audience: str) -> str:
    """
    Fetches a Google OpenID Connect (OIDC) ID Token.
    On Google Cloud (Cloud Run, GKE, GCE), this queries the local Metadata Server.
    Locally, it uses the active Application Default Credentials (ADC).
    """
    try:
        # We need to pass a Request object to fetch the token
        auth_request = google.auth.transport.requests.Request()
        # Audience should be the base URL of the smart router (e.g., https://my-router-xxxx.a.run.app)
        token = google.oauth2.id_token.fetch_id_token(auth_request, audience)
        return token
    except Exception as exc:
        logger.error(f"Failed to retrieve Google OIDC identity token: {exc}")
        raise RuntimeError(
            f"Authentication failure: Unable to obtain Google ID token. Ensure service account permissions are correct. Detail: {exc}"
        )

@app.get("/health")
async def health_check():
    return {"status": "healthy", "router_target": ROUTER_URL}

@app.post("/generate", response_model=GenerateResponse)
async def generate_content(payload: GenerateRequest):
    """
    Zero-key prompt submission. Retrieves an OIDC Token for the target router and posts
    directly to gemini-2.5-flash-lite.
    """
    # The target router's OIDC token audience is the base router URL
    token = get_oidc_token(ROUTER_URL)

    # Target the cost-effective gemini-2.5-flash-lite model
    url = f"{ROUTER_URL}/v1/models/gemini-2.5-flash-lite:generateContent"

    # Set headers. Google OIDC identity tokens are sent as standard Bearer tokens.
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {token}"
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

    logger.info(f"Sending OIDC authenticated request to smart router: {url}")
    
    async with httpx.AsyncClient() as client:
        try:
            response = await client.post(
                url,
                json=data,
                headers=headers,
                timeout=30.0
            )
            
            # If the router returned rate limits (429) or auth errors (401), forward details
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
            
            # Extract text
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

            # Retrieve router-injected headers
            model_routed = response.headers.get("X-Routed-Model") or "gemini-2.5-flash-lite"
            client_tier = response.headers.get("X-Client-Tier")
            latency = response.headers.get("X-Response-Time")

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
