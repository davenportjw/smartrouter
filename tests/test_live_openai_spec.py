import hashlib
import os
import secrets
import time
import pytest
from openai import OpenAI
from google.cloud import firestore

PROJECT_ID = os.environ.get("GOOGLE_CLOUD_PROJECT", "").strip()
SERVICE_URL = os.environ.get("SERVICE_URL", "").strip().rstrip("/")

TEST_CLIENT_ID = "pytest-openai-spec-client"
TEST_APP_ID = "pytest-openai-spec-app"


def _hash_key(key: str) -> str:
    return hashlib.sha256(key.encode("utf-8")).hexdigest()


@pytest.fixture(scope="module")
def live_openai_client():
    """Seeds temporary client/app/key in Firestore, yields an OpenAI SDK client, and cleans up after."""
    if not PROJECT_ID or not SERVICE_URL:
        pytest.skip("GOOGLE_CLOUD_PROJECT and SERVICE_URL must be set for live OpenAI spec tests")
    test_api_key = f"gr_pytest_{secrets.token_hex(16)}"
    db = firestore.Client(project=PROJECT_ID)
    key_hash = _hash_key(test_api_key)

    try:
        db.collection("clients").document(TEST_CLIENT_ID).set(
            {
                "id": TEST_CLIENT_ID,
                "name": "Pytest OpenAI Spec Client",
                "tier": "premium",
                "rpm": 120,
                "tpm": 100000,
                "priority": "high",
            }
        )
        db.collection("apps").document(TEST_APP_ID).set(
            {
                "id": TEST_APP_ID,
                "client_id": TEST_CLIENT_ID,
                "name": "Pytest OpenAI Spec App",
                "rpm": 120,
                "tpm": 100000,
                "priority": "high",
                "complexity": {
                    "enabled": True,
                    "always_override": False,
                    "simple_model": "gemini-3.5-flash-lite",
                    "medium_model": "gemini-3.8-flash",
                    "complex_model": "gemini-3.1-pro",
                    "simple_char_limit": 80,
                    "medium_char_limit": 300,
                },
            }
        )
        db.collection("api_keys").document(key_hash).set(
            {
                "key_hash": key_hash,
                "client_id": TEST_CLIENT_ID,
                "app_id": TEST_APP_ID,
                "status": "active",
            }
        )

        # Wait for Firestore snapshot listener on Cloud Run to sync
        time.sleep(4)

        client = OpenAI(
            api_key=test_api_key,
            base_url=f"{SERVICE_URL}/v1",
            default_headers={
                "x-goog-api-key": test_api_key,
                "X-Client-App-ID": TEST_APP_ID,
            },
        )
        yield client
    finally:
        db.collection("api_keys").document(key_hash).delete()
        db.collection("apps").document(TEST_APP_ID).delete()
        db.collection("clients").document(TEST_CLIENT_ID).delete()


def test_openai_sdk_models_list_and_retrieve(live_openai_client: OpenAI):
    models_page = live_openai_client.models.list()
    model_ids = [m.id for m in models_page.data]
    assert "gemini-dynamic" in model_ids
    assert "gemini-3.8-flash" in model_ids

    single = live_openai_client.models.retrieve("gemini-3.8-flash")
    assert single.id == "gemini-3.8-flash"
    assert single.object == "model"


def test_openai_sdk_chat_completions_non_streaming(live_openai_client: OpenAI):
    completion = live_openai_client.chat.completions.create(
        model="gemini-dynamic",
        messages=[
            {"role": "system", "content": "Answer in one short sentence."},
            {"role": "user", "content": "Hi!"},
        ],
    )
    assert completion.object == "chat.completion"
    assert len(completion.choices) > 0
    assert completion.choices[0].message.role == "assistant"
    assert len(completion.choices[0].message.content.strip()) > 0


def test_openai_sdk_chat_completions_streaming(live_openai_client: OpenAI):
    stream = live_openai_client.chat.completions.create(
        model="gemini-3.8-flash",
        messages=[{"role": "user", "content": "Say hello in two words."}],
        stream=True,
    )
    chunks = []
    for chunk in stream:
        if chunk.choices and chunk.choices[0].delta and chunk.choices[0].delta.content:
            chunks.append(chunk.choices[0].delta.content)
    full_text = "".join(chunks).strip()
    assert len(full_text) > 0
