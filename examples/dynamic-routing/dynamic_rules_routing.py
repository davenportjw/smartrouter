#!/usr/bin/env python3
"""
Gemini Smart Router - Dynamic Rules-Based Routing Example

This script demonstrates how to trigger custom Routing Rules in the Smart Router
based on incoming custom headers (e.g. X-Route-Priority) or client attributes.
"""

import os
import json
import sys
import httpx

# Configuration
ROUTER_URL = os.getenv("ROUTER_URL", "http://localhost:8080").rstrip("/")
API_KEY = os.getenv("ROUTER_API_KEY", "gr_key_enterprise_123456789")

def send_rules_request(requested_model: str, custom_headers: dict, description: str):
    print("-" * 70)
    print(f"Scenario: {description}")
    print(f"Requested Model: {requested_model}")
    print(f"Custom Headers Passed: {custom_headers}")

    url = f"{ROUTER_URL}/v1/models/{requested_model}:generateContent"
    
    # Standard required headers
    headers = {
        "Content-Type": "application/json",
        "x-goog-api-key": API_KEY,
        "X-Client-App-ID": "prod-app-main" # Seeks authorized App
    }

    # Inject user custom routing/rule headers
    for k, v in custom_headers.items():
        headers[k] = v

    data = {
        "contents": [
            {
                "role": "user",
                "parts": [
                    {"text": "Give me a 1-sentence tagline for an eco-friendly water bottle."}
                ]
            }
        ]
    }

    try:
        with httpx.Client() as client:
            response = client.post(url, json=data, headers=headers, timeout=45.0)
            
            if response.status_code != 200:
                print(f"❌ Error (Status {response.status_code}): {response.text}")
                return

            res_json = response.json()
            
            # Retrieve headers returned by the smart router
            req_model = response.headers.get("X-Requested-Model", "Unknown")
            routed_model = response.headers.get("X-Routed-Model", "Unknown")
            routed_loc = response.headers.get("X-Routed-Model-Location", "global")
            client_tier = response.headers.get("X-Client-Tier", "Unknown")

            # Get output text
            try:
                text = res_json["candidates"][0]["content"]["parts"][0]["text"]
            except (KeyError, IndexError):
                text = "No text generated."

            print(f"✅ Success!")
            print(f"👉 Requested Model : {req_model}")
            print(f"👉 Routed Model    : \033[1;32m{routed_model}\033[0m")
            print(f"👉 Routed Location : \033[1;34m{routed_loc}\033[0m")
            print(f"👉 Client Tier      : {client_tier}")
            print(f"👉 Response Snippet: \"{text.strip()[:100].replace(chr(10), ' ')}...\"")

    except httpx.RequestError as exc:
        print(f"❌ Request failed: {exc}")

def main():
    print("======================================================================")
    print("     G E M I N I   S M A R T   R O U T E R   -   R U L E S   R O U T I N G")
    print("======================================================================")
    print(f"Targeting Router URL: {ROUTER_URL}")
    print(f"Using API Key       : {API_KEY[:10]}...")
    
    # 1. Requesting gemini-1.5-pro with no headers
    # Standard routing rules should rewrite '*' to default 'gemini-2.5-flash'
    send_rules_request(
        "gemini-1.5-pro", 
        {}, 
        "Standard Request (No custom routing headers)"
    )

    # 2. Requesting gemini-1.5-pro WITH 'X-Route-Priority: gold' header
    # In local_db.json, we have seeded a rule that maps X-Route-Priority: gold
    # requesting gemini-1.5-pro to gemini-2.5-pro!
    send_rules_request(
        "gemini-1.5-pro", 
        {"X-Route-Priority": "gold"}, 
        "Rules-Based Request (Targeting rule: X-Route-Priority = gold)"
    )

if __name__ == "__main__":
    main()
