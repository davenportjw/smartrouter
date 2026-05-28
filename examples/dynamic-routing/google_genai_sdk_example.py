#!/usr/bin/env python3
"""
Gemini Smart Router - Google GenAI SDK Integration Example

This script demonstrates how to integrate the official Google GenAI SDK with the 
Gemini Smart Router by initializing a custom API client that routes calls to the 
Smart Router's local or production endpoint.
"""

import os
import sys

try:
    # Import official Google GenAI Client
    from google import genai
    from google.genai import types
except ImportError:
    print("❌ Error: The 'google-genai' package is required to run this example.")
    print("Please install it using: pip install google-genai")
    sys.exit(1)

# Configuration
ROUTER_URL = os.getenv("ROUTER_URL", "http://localhost:8080").rstrip("/")
API_KEY = os.getenv("ROUTER_API_KEY", "gr_key_enterprise_123456789")

def run_sdk_example():
    print("======================================================================")
    print("   G E M I N I   S M A R T   R O U T E R   -   S D K   E X A M P L E")
    print("======================================================================")
    print(f"Targeting Router Base URL: {ROUTER_URL}")
    print(f"Using Client API Key     : {API_KEY[:10]}...\n")

    # Initialize official Google GenAI Client pointing to Smart Router
    # We set http_options.api_endpoint to route all requests through our proxy backend,
    # and use our Smart Router generated API key for client authentication.
    client = genai.Client(
        api_key=API_KEY,
        http_options={
            "api_endpoint": ROUTER_URL
        }
    )

    # Target the virtual dynamic routing model
    model_id = "gemini-dynamic"

    print("--- Prompt 1: Simple Task (Should route to gemini-2.5-flash-lite) ---")
    prompt_simple = "What is 2 + 2? Be extremely brief."
    print(f"Prompt: \"{prompt_simple}\"")
    
    try:
        # Standard SDK GenerateContent invocation
        response = client.models.generate_content(
            model=model_id,
            contents=prompt_simple,
            config=types.GenerateContentConfig(
                # Seed required app identifier header for dynamic schema mapping
                http_headers={
                    "X-Client-App-ID": "prod-app-main"
                }
            )
        )
        
        print(f"✅ Success!")
        print(f"👉 Response Text  : {response.text.strip()}")
        
    except Exception as e:
        print(f"❌ SDK Request Failed: {e}")

    print("\n--- Prompt 2: Complex Coding Task (Should route to gemini-2.5-pro) ---")
    prompt_complex = (
        "Write a complete thread-safe Singleton pattern in Go. "
        "Include comments and explanations of why sync.Once is used."
    )
    print(f"Prompt: \"{prompt_complex[:60]}...\"")
    
    try:
        response = client.models.generate_content(
            model=model_id,
            contents=prompt_complex,
            config=types.GenerateContentConfig(
                http_headers={
                    "X-Client-App-ID": "prod-app-main"
                }
            )
        )
        
        print(f"✅ Success!")
        snippet = response.text.strip()
        if len(snippet) > 120:
            snippet = snippet[:120] + "..."
        print(f"👉 Response Text  : \"{snippet}\"")
        
    except Exception as e:
        print(f"❌ SDK Request Failed: {e}")

if __name__ == "__main__":
    run_sdk_example()
