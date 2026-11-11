#!/usr/bin/env python3
"""
Gemini Smart Router - Dynamic Complexity & Adaptive Smart Routing Example

Demonstrates:
1. Fast Decision Models (Jev Structured Rules & CLM Contrastive Language Model) via 'gemini-dynamic'
2. Per-Prompt Routing Overrides (X-Router-Model, X-Router-Complexity, X-Router-Engine)
3. Sticky Multi-Turn Session Continuation (X-Session-ID, X-Session-Policy: lock)
4. Autonomous Agent Task Switching (X-Agent-Task: debugging, code_generation, triage)
"""

import os
import json
import sys
import httpx

# Configuration
ROUTER_URL = os.getenv("ROUTER_URL", "http://localhost:8080").rstrip("/")
API_KEY = os.getenv("ROUTER_API_KEY", "").strip()
if not API_KEY:
    print("❌ Error: ROUTER_API_KEY environment variable is required.")
    sys.exit(1)

def send_router_request(prompt: str, description: str, custom_headers: dict = None, model: str = "gemini-dynamic"):
    print("-" * 75)
    print(f"Scenario: {description}")
    print(f"Prompt  : \"{prompt[:60]}...\" (Length: {len(prompt)} chars)")

    url = f"{ROUTER_URL}/v1/models/{model}:generateContent"
    headers = {
        "Content-Type": "application/json",
        "x-goog-api-key": API_KEY,
        "X-Client-App-ID": "prod-app-main",
    }
    if custom_headers:
        headers.update(custom_headers)

    data = {
        "contents": [
            {
                "role": "user",
                "parts": [
                    {"text": prompt}
                ]
            }
        ]
    }

    try:
        with httpx.Client() as client:
            response = client.post(url, json=data, headers=headers, timeout=45.0)

            if response.status_code == 400 and "dynamic complexity routing is not enabled" in response.text:
                print("❌ Error: Dynamic complexity routing is not enabled for this application.")
                return

            if response.status_code != 200:
                print(f"❌ Error (Status {response.status_code}): {response.text}")
                return

            res_json = response.json()

            # Extract response metadata injected by the router
            requested_model = response.headers.get("X-Requested-Model", model)
            routed_model = response.headers.get("X-Routed-Model", "Unknown")
            engine_used = response.headers.get("X-Router-Engine-Used", "heuristics")
            tier = response.headers.get("X-Router-Complexity-Tier", "standard")
            session_id = response.headers.get("X-Session-ID", "none")
            turn = response.headers.get("X-Session-Turn", "0")
            agent_task = response.headers.get("X-Agent-Task", "none")
            routed_loc = response.headers.get("X-Routed-Model-Location", "global")

            # Extract output text snippet
            try:
                text = res_json["candidates"][0]["content"]["parts"][0]["text"]
            except (KeyError, IndexError):
                text = "No text generated."

            print(f"✅ Success!")
            print(f"👉 Requested Model : {requested_model}")
            print(f"👉 Routed Model    : \033[1;32m{routed_model}\033[0m")
            print(f"👉 Engine Used     : \033[1;35m{engine_used}\033[0m (Tier: {tier})")
            if session_id != "none":
                print(f"👉 Session Lock    : \033[1;36m{session_id}\033[0m (Turn: {turn})")
            if agent_task != "none":
                print(f"👉 Agent Task      : \033[1;33m{agent_task}\033[0m")
            print(f"👉 Routed Location : {routed_loc}")
            print(f"👉 Output Snippet  : \"{text.strip()[:90].replace(chr(10), ' ')}...\"")

    except httpx.RequestError as exc:
        print(f"❌ Request failed: {exc}")

def main():
    print("==========================================================================")
    print("     G E M I N I   S M A R T   R O U T E R   -   D Y N A M I C   R O U T I N G")
    print("==========================================================================")
    print(f"Targeting Router URL: {ROUTER_URL}")
    print(f"Using API Key       : {API_KEY[:10]}...")

    # 1. Fast Jev Decision Model Classification
    send_router_request(
        "Hi, what is the capital of Japan?",
        "1. Jev Model -> Simple Query (Target: gemini-3.8-flash-lite)",
        custom_headers={"X-Router-Engine": "jev"}
    )

    # 2. Fast CLM Contrastive Model Classification
    send_router_request(
        "Compare the memory layout and allocation strategies of slice vs array in Go.",
        "2. CLM Contrastive Model -> Medium Reasoning (Target: gemini-3.8-flash)",
        custom_headers={"X-Router-Engine": "clm"}
    )

    # 3. Per-Prompt Override (Immediate model pinning)
    send_router_request(
        "Calculate 1+1",
        "3. Per-Prompt Override -> Force gemini-3.8-pro via X-Router-Model",
        custom_headers={"X-Router-Model": "gemini-3.8-pro"}
    )

    # 4. Multi-Turn Session Continuation (Sticky Session Lock)
    session_id = "session-agent-interactive-001"
    send_router_request(
        "```go\nfunc DeepAlgorithm(matrix [][]int) int { return 42 }\n```\nAnalyze performance.",
        "4a. Session Turn 1 -> Classifies as complex and locks model to session",
        custom_headers={
            "X-Session-ID": session_id,
            "X-Session-Policy": "lock"
        }
    )
    send_router_request(
        "Thanks! Can you add unit tests for that?",
        "4b. Session Turn 2 -> Short prompt inherits locked gemini-3.8-pro model",
        custom_headers={
            "X-Session-ID": session_id,
            "X-Session-Policy": "lock"
        }
    )

    # 5. Autonomous Agent Task Switching
    send_router_request(
        "Inspect this stack trace and identify why the goroutine leaked.",
        "5. Agent Task Switching -> X-Agent-Task: debugging routes to complex tier",
        custom_headers={
            "X-Agent-Task": "debugging"
        }
    )

if __name__ == "__main__":
    main()
