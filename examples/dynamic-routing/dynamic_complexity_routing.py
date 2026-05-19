#!/usr/bin/env python3
"""
Gemini Smart Router - Dynamic Complexity Routing Example

This script demonstrates how to use the Virtual Model 'gemini-dynamic' to 
automatically classify prompt complexity and route requests to the most 
cost-effective model (gemini-2.5-flash-lite, gemini-2.5-flash, or gemini-2.5-pro).
"""

import os
import json
import sys
import httpx

# Configuration
ROUTER_URL = os.getenv("ROUTER_URL", "http://localhost:8080").rstrip("/")
# In local development (LOCAL_DEV=true), the server uses local db credentials.
# The pre-seeded key hash is for 'gr_key_enterprise_123456789' or similar.
API_KEY = os.getenv("ROUTER_API_KEY", "gr_key_enterprise_123456789")

def send_dynamic_request(prompt: str, description: str):
    print("-" * 70)
    print(f"Prompt Type: {description}")
    print(f"Prompt: \"{prompt[:60]}...\" (Length: {len(prompt)} chars)")

    url = f"{ROUTER_URL}/v1/models/gemini-dynamic:generateContent"
    headers = {
        "Content-Type": "application/json",
        "x-goog-api-key": API_KEY
    }

    # Set default client app ID for declarative custom headers in local db
    headers["X-Client-App-ID"] = "prod-app-main"

    data = {
        "contents": [
            {
                "parts": [
                    {"text": prompt}
                ]
            }
        ]
    }

    try:
        with httpx.Client() as client:
            response = client.post(url, json=data, headers=headers, timeout=15.0)
            
            if response.status_code == 400 and "dynamic complexity routing is not enabled" in response.text:
                print("❌ Error: Dynamic complexity routing is not enabled for this application.")
                print("Please enable complexity routing in the admin panel or update local_db.json.")
                return

            if response.status_code != 200:
                print(f"❌ Error (Status {response.status_code}): {response.text}")
                return

            res_json = response.json()
            
            # Extract response metadata injected by the router
            requested_model = response.headers.get("X-Requested-Model", "gemini-dynamic")
            routed_model = response.headers.get("X-Routed-Model", "Unknown")
            client_tier = response.headers.get("X-Client-Tier", "Unknown")
            app_id = response.headers.get("X-App-ID", "Unknown")

            # Get output text
            try:
                text = res_json["candidates"][0]["content"]["parts"][0]["text"]
            except (KeyError, IndexError):
                text = "No text generated."

            print(f"✅ Success!")
            print(f"👉 Requested Model : {requested_model}")
            print(f"👉 Routed Model    : \033[1;32m{routed_model}\033[0m")
            print(f"👉 Client Tier      : {client_tier}")
            print(f"👉 App ID          : {app_id}")
            print(f"👉 Response Snippet: \"{text.strip()[:100].replace(chr(10), ' ')}...\"")

    except httpx.RequestError as exc:
        print(f"❌ Request failed: {exc}")

def main():
    print("======================================================================")
    print("     G E M I N I   S M A R T   R O U T E R   -   D Y N A M I C   R O U T I N G")
    print("======================================================================")
    print(f"Targeting Router URL: {ROUTER_URL}")
    print(f"Using API Key       : {API_KEY[:10]}...")
    
    # 1. Simple greeting - should route to gemini-2.5-flash-lite
    simple_prompt = "Hello! Quick question: what is the chemical symbol for gold?"
    send_dynamic_request(simple_prompt, "Simple / Factual lookup")

    # 2. Medium task - should route to gemini-2.5-flash
    medium_prompt = (
        "Please summarize the core benefits of microservices architectures compared "
        "to monolithic codebases. Write a summary of about 40 to 50 words."
    )
    send_dynamic_request(medium_prompt, "Medium / Summarization task")

    # 3. Complex prompt - should route to gemini-2.5-pro
    complex_prompt = (
        "Solve this algorithmic problem and optimize it for O(N) time complexity. "
        "Explain each step thoroughly, drawing on deep software engineering principles. "
        "Problem: Given an array of integers, find the contiguous subarray which has "
        "the largest sum and return its sum. Provide complete, clean, and idiomatic Go code."
    )
    send_dynamic_request(complex_prompt, "Complex / Algorithmic reasoning task")

if __name__ == "__main__":
    main()
