#!/bin/bash
# Gemini Smart Router automated deployment script

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO] $1${NC}"
}

log_success() {
    echo -e "${GREEN}[SUCCESS] $1${NC}"
}

log_error() {
    echo -e "${RED}[ERROR] $1${NC}"
}

# Helper function to update/add environment variables in .env safely
update_env_var() {
    local key="$1"
    local value="$2"
    if grep -q "^${key}=" .env; then
        # Escape value for sed
        local escaped_value=$(echo "$value" | sed 's/[\/&]/\\&/g')
        sed -i.bak "s/^${key}=.*/${key}=\"${escaped_value}\"/" .env && rm .env.bak
    else
        echo "${key}=\"${value}\"" >> .env
    fi
    # Update current shell's active env var
    export "${key}=${value}"
}

# Helper function to verify if a Firebase Web API Key is active and valid on IdentityToolkit
verify_firebase_api_key() {
    local candidate_key="$1"
    if [ -z "$candidate_key" ] || [ "$candidate_key" = "AIzaSyYourFirebaseWebApiKey" ] || [ "$candidate_key" = "null" ]; then
        return 1
    fi
    local probe_code
    probe_code=$(curl -s -w "%{http_code}" -o /tmp/fb_key_probe.json \
        -X POST "https://identitytoolkit.googleapis.com/v1/accounts:createAuthUri?key=${candidate_key}" \
        -H "Content-Type: application/json" \
        -d '{"continueUri":"http://localhost","providerId":"google.com"}' 2>/dev/null || echo "000")
    if [ "$probe_code" = "200" ]; then
        return 0
    fi
    return 1
}

# Check, enable, and retrieve Firebase configuration automatically
setup_firebase_config() {
    local needs_refresh=false
    if [ -z "$FIREBASE_API_KEY" ] || [ "$FIREBASE_API_KEY" = "AIzaSyYourFirebaseWebApiKey" ] || \
       [ -z "$FIREBASE_APP_ID" ] || [ "$FIREBASE_APP_ID" = "1:1234:web:abcd" ]; then
        log_info "Firebase credentials missing or default in .env. Initiating auto-configuration..."
        needs_refresh=true
    elif ! verify_firebase_api_key "$FIREBASE_API_KEY"; then
        log_info "Configured FIREBASE_API_KEY in .env is expired or invalid on IdentityToolkit. Refreshing from GCP Firebase Web App..."
        needs_refresh=true
    fi

    if [ "$needs_refresh" = true ]; then
        # Fetch project number
        log_info "Retrieving GCP project details for '$GOOGLE_CLOUD_PROJECT'..."
        local project_num
        project_num=$(gcloud projects describe "$GOOGLE_CLOUD_PROJECT" --format="value(projectNumber)" 2>/dev/null || true)
        
        if [ -z "$project_num" ]; then
            log_error "Unable to retrieve project number using gcloud. Make sure you have active credentials."
            return 1
        fi
        log_info "Project Number: $project_num"

        # Get active GCP authorization token (ADC or active user)
        local auth_token
        auth_token=$(gcloud auth application-default print-access-token 2>/dev/null || true)
        if [ -z "$auth_token" ]; then
            auth_token=$(gcloud auth print-access-token 2>/dev/null || true)
        fi
        
        if [ -z "$auth_token" ]; then
            log_error "Failed to acquire an authentication token. Run 'gcloud auth application-default login' or 'gcloud auth login'."
            return 1
        fi

        # Validate Firebase project enablement
        log_info "Verifying Firebase integration status on project..."
        local check_code
        check_code=$(curl -s -w "%{http_code}" -o /tmp/fb_check_resp.json \
            -H "Authorization: Bearer $auth_token" \
            -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
            "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}")

        if [ "$check_code" -ne 200 ]; then
            log_info "Firebase not active on this project. Enabling Firebase..."
            local enable_code
            enable_code=$(curl -s -w "%{http_code}" -o /tmp/fb_add_resp.json \
                -X POST -H "Authorization: Bearer $auth_token" \
                -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
                -H "Content-Type: application/json" \
                -d '{}' \
                "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}:addFirebase")
            
            if [ "$enable_code" -eq 200 ] || [ "$enable_code" -eq 202 ]; then
                log_success "Firebase successfully linked to GCP project!"
                sleep 3
            else
                log_error "Failed to link Firebase. HTTP response: $enable_code. Specs: $(cat /tmp/fb_add_resp.json 2>/dev/null)"
                return 1
            fi
        else
            log_info "Firebase is already enabled on this project."
        fi

        # Locate suitable Web App
        log_info "Locating registered Web Application resources..."
        local apps_list
        apps_list=$(curl -s -H "Authorization: Bearer $auth_token" \
            -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
            "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}/webApps")

        local app_id=""
        app_id=$(echo "$apps_list" | jq -r '.apps[] | select(.displayName == "Gemini Router Admin") | .appId // empty' | head -n 1)
        if [ -z "$app_id" ]; then
            app_id=$(echo "$apps_list" | jq -r '.apps[0].appId // empty')
        fi

        # Register new Web App if none found
        if [ -z "$app_id" ]; then
            log_info "No active Web App found. Registering 'Gemini Router Admin'..."
            local register_code
            register_code=$(curl -s -w "%{http_code}" -o /tmp/fb_register_resp.json \
                -X POST -H "Authorization: Bearer $auth_token" \
                -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
                -H "Content-Type: application/json" \
                -d '{"displayName": "Gemini Router Admin"}' \
                "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}/webApps")

            if [ "$register_code" -eq 200 ] || [ "$register_code" -eq 202 ]; then
                sleep 3
                apps_list=$(curl -s -H "Authorization: Bearer $auth_token" \
                    -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
                    "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}/webApps")
                app_id=$(echo "$apps_list" | jq -r '.apps[] | select(.displayName == "Gemini Router Admin") | .appId // empty' | head -n 1)
                if [ -z "$app_id" ]; then
                    app_id=$(echo "$apps_list" | jq -r '.apps[0].appId // empty')
                fi
            else
                log_error "Failed to register Web App. Response: $(cat /tmp/fb_register_resp.json 2>/dev/null)"
                return 1
            fi
        fi

        if [ -z "$app_id" ]; then
            log_error "Failed to resolve app ID for Firebase Web Client."
            return 1
        fi
        log_info "Using Web App ID: $app_id"

        # Pull SDK Configurations
        log_info "Fetching Web App SDK configurations..."
        local config_data
        config_data=$(curl -s -H "Authorization: Bearer $auth_token" \
            -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
            "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}/webApps/${app_id}/config")

        local api_key=$(echo "$config_data" | jq -r '.apiKey // empty')
        local auth_domain=$(echo "$config_data" | jq -r '.authDomain // empty')
        local storage_bucket=$(echo "$config_data" | jq -r '.storageBucket // empty')

        if ! verify_firebase_api_key "$api_key"; then
            log_info "Web App config key missing or inactive; searching active GCP API keys for IdentityToolkit..."
            local keys_json
            keys_json=$(curl -s -H "Authorization: Bearer $auth_token" \
                -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
                "https://apikeys.googleapis.com/v2/projects/${GOOGLE_CLOUD_PROJECT}/locations/global/keys")
            for kname in $(echo "$keys_json" | jq -r '.keys[]?.name // empty'); do
                local kstr
                kstr=$(curl -s -H "Authorization: Bearer $auth_token" \
                    -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
                    "https://apikeys.googleapis.com/v2/${kname}/keyString" | jq -r '.keyString // empty')
                if verify_firebase_api_key "$kstr"; then
                    api_key="$kstr"
                    break
                fi
            done
        fi

        if [ -z "$api_key" ] || [ "$api_key" = "null" ]; then
            log_error "No valid API key linked with your Web App. Please verify Credentials API configurations in GCP console."
            return 1
        fi

        if [ -z "$storage_bucket" ] || [ "$storage_bucket" = "null" ]; then
            storage_bucket="${GOOGLE_CLOUD_PROJECT}.firebasestorage.app"
        fi
        if [ -z "$auth_domain" ] || [ "$auth_domain" = "null" ]; then
            auth_domain="${GOOGLE_CLOUD_PROJECT}.firebaseapp.com"
        fi

        log_info "Writing resolved configurations back to .env..."
        update_env_var "FIREBASE_API_KEY" "$api_key"
        export FIREBASE_WEB_API_KEY="$api_key"
        update_env_var "FIREBASE_AUTH_DOMAIN" "$auth_domain"
        update_env_var "FIREBASE_PROJECT_ID" "$GOOGLE_CLOUD_PROJECT"
        update_env_var "FIREBASE_STORAGE_BUCKET" "$storage_bucket"
        update_env_var "FIREBASE_MESSAGING_SENDER_ID" "$project_num"
        update_env_var "FIREBASE_APP_ID" "$app_id"

        log_success "Firebase credentials successfully updated and verified automatically!"
    else
        log_info "Firebase credentials already configured and verified in .env."
    fi
}

# 1. Load environmental variables
if [ ! -f .env ]; then
    log_error ".env file not found in root directory. Please copy .env.sample to .env and fill it out."
    exit 1
fi

log_info "Loading environment variables from .env..."
while IFS='=' read -r key val || [ -n "$key" ]; do
    # Skip comments and empty lines
    if [[ "$key" =~ ^[[:space:]]*# ]] || [[ -z "${key// }" ]]; then
        continue
    fi
    key=$(echo "$key" | xargs)
    # Strip surrounding single or double quotes from value
    val="${val#\"}"
    val="${val%\"}"
    val="${val#\'}"
    val="${val%\'}"
    export "$key=$val"
done < .env

# Validate critical baseline variables
if [ -z "$GOOGLE_CLOUD_PROJECT" ]; then
    log_error "GOOGLE_CLOUD_PROJECT is missing in .env"
    exit 1
fi

DEPLOY_REGION="${GEMINI_LOCATION:-${LOCATION:-us-central1}}"
if [ "$DEPLOY_REGION" = "global" ] || [ "$DEPLOY_REGION" = "us" ]; then
    CLOUD_RUN_REGION="us-central1"
else
    CLOUD_RUN_REGION="$DEPLOY_REGION"
fi

# Invoke automated Firebase configuration
setup_firebase_config

# Generate secure BACKEND_SHARED_SECRET if it is missing
if [ -z "$BACKEND_SHARED_SECRET" ]; then
    log_info "Generating a secure BACKEND_SHARED_SECRET..."
    RAND_SECRET=$(LC_ALL=C tr -dc 'a-zA-Z0-9' < /dev/urandom | fold -w 32 | head -n 1)
    update_env_var "BACKEND_SHARED_SECRET" "$RAND_SECRET"
fi

ADMIN_API_SECRET="${ADMIN_API_SECRET:-$BACKEND_SHARED_SECRET}"
FIREBASE_WEB_API_KEY="${FIREBASE_WEB_API_KEY:-$FIREBASE_API_KEY}"
ALLOWED_EMAIL_DOMAINS="${ALLOWED_EMAIL_DOMAINS:-${ALLOWED_AUTH_DOMAINS:-$ALLOWED_DOMAINS}}"
ALLOWED_AUTH_DOMAINS="${ALLOWED_AUTH_DOMAINS:-$ALLOWED_EMAIL_DOMAINS}"
ALLOWED_DOMAINS="${ALLOWED_DOMAINS:-$ALLOWED_EMAIL_DOMAINS}"

# Validate Firebase configurations are loaded
if [ -z "$FIREBASE_API_KEY" ] || [ "$FIREBASE_API_KEY" = "AIzaSyYourFirebaseWebApiKey" ] || \
   [ -z "$FIREBASE_APP_ID" ] || [ "$FIREBASE_APP_ID" = "1:1234:web:abcd" ]; then
    log_error "Missing critical Firebase credentials. Please fill out the fields in .env manually or authorize gcloud."
    exit 1
fi

# 2. Provision Infrastructure via Terraform
log_info "Starting Terraform initialization and provisioning..."
cd terraform

terraform init

if [ -z "$ALLOWED_EMAIL_DOMAINS" ]; then
    log_error "ALLOWED_EMAIL_DOMAINS is missing or empty in .env. You must configure at least one authorized domain or specific email address for administrative dashboard sign-in."
    exit 1
fi

log_info "Applying Terraform configuration with authorized domains: $ALLOWED_EMAIL_DOMAINS"
terraform apply -auto-approve \
  -var="project_id=$GOOGLE_CLOUD_PROJECT" \
  -var="region=$CLOUD_RUN_REGION" \
  -var="firebase_api_key=$FIREBASE_API_KEY" \
  -var="firebase_auth_domain=$FIREBASE_AUTH_DOMAIN" \
  -var="firebase_storage_bucket=$FIREBASE_STORAGE_BUCKET" \
  -var="firebase_messaging_sender_id=$FIREBASE_MESSAGING_SENDER_ID" \
  -var="firebase_app_id=$FIREBASE_APP_ID" \
  -var="allowed_email_domains=$ALLOWED_EMAIL_DOMAINS"

cd ..

# 3. Ensure populated .env secrets are synced in GCP Secret Manager and build --set-secrets bindings for Cloud Run
sync_secret_to_gsm() {
    local secret_name="$1"
    local secret_val="$2"
    if [ -z "$secret_val" ]; then
        return 0
    fi
    if ! gcloud secrets describe "$secret_name" --project="$GOOGLE_CLOUD_PROJECT" >/dev/null 2>&1; then
        log_info "Creating Secret Manager secret '$secret_name'..."
        gcloud secrets create "$secret_name" --replication-policy="automatic" --project="$GOOGLE_CLOUD_PROJECT" --quiet
    fi
    printf "%s" "$secret_val" | gcloud secrets versions add "$secret_name" --data-file=- --project="$GOOGLE_CLOUD_PROJECT" --quiet >/dev/null
    gcloud secrets add-iam-policy-binding "$secret_name" \
        --member="serviceAccount:gemini-router-runner@${GOOGLE_CLOUD_PROJECT}.iam.gserviceaccount.com" \
        --role="roles/secretmanager.secretAccessor" \
        --project="$GOOGLE_CLOUD_PROJECT" --quiet >/dev/null
}

JEV_RESOLVED="${JEV_API_KEY:-${TYPESAFE_API_KEY:-}}"
FASTINO_RESOLVED="${FASTINO_API_KEY:-${PIONEER_API_KEY:-}}"
CLM_RESOLVED="${CLM_API_KEY:-${CONTRASTIVE_LM_API_KEY:-}}"

log_info "Synchronizing populated .env secrets with Google Cloud Secret Manager..."
sync_secret_to_gsm "BACKEND_SHARED_SECRET" "$BACKEND_SHARED_SECRET"
sync_secret_to_gsm "JEV_API_KEY" "$JEV_RESOLVED"
sync_secret_to_gsm "FASTINO_API_KEY" "$FASTINO_RESOLVED"
sync_secret_to_gsm "CLM_API_KEY" "$CLM_RESOLVED"

SECRET_MOUNTS="BACKEND_SHARED_SECRET=BACKEND_SHARED_SECRET:latest,ADMIN_API_SECRET=BACKEND_SHARED_SECRET:latest"
for opt_secret in JEV_API_KEY FASTINO_API_KEY CLM_API_KEY; do
    if gcloud secrets versions access latest --secret="$opt_secret" --project="$GOOGLE_CLOUD_PROJECT" >/dev/null 2>&1; then
        SECRET_MOUNTS="${SECRET_MOUNTS},${opt_secret}=${opt_secret}:latest"
    fi
done

# 4. Generate Templ Templates
log_info "Compiling Go HTML Templ components..."
go run github.com/a-h/templ/cmd/templ@v0.3.1070 generate

# 5. Build containers on Cloud Build and Deploy to Cloud Run
log_info "Building Smart Router Backend Container via Cloud Build..."
gcloud builds submit --config cloudbuild-backend.yaml --project "$GOOGLE_CLOUD_PROJECT" .

log_info "Deploying Smart Router Backend to Cloud Run with Secret Manager mounts..."
gcloud run deploy gemini-smart-router \
  --image "gcr.io/$GOOGLE_CLOUD_PROJECT/smart-router-backend" \
  --region "$CLOUD_RUN_REGION" \
  --service-account "gemini-router-runner@$GOOGLE_CLOUD_PROJECT.iam.gserviceaccount.com" \
  --project "$GOOGLE_CLOUD_PROJECT" \
  --update-env-vars="^@^GOOGLE_CLOUD_PROJECT=${GOOGLE_CLOUD_PROJECT}@GEMINI_LOCATION=${DEPLOY_REGION}@LOCATION=${DEPLOY_REGION}" \
  --set-secrets="${SECRET_MOUNTS}" \
  --quiet

BACKEND_URL=$(gcloud run services describe gemini-smart-router --region "$CLOUD_RUN_REGION" --format="value(status.url)" --project="$GOOGLE_CLOUD_PROJECT" 2>/dev/null || true)
PROXY_PUBLIC_URL="${PROXY_PUBLIC_URL:-$BACKEND_URL}"
BACKEND_API_URL="${BACKEND_API_URL:-$BACKEND_URL}"

log_info "Building Smart Router Frontend UI Container via Cloud Build..."
gcloud builds submit --config cloudbuild-frontend.yaml --project "$GOOGLE_CLOUD_PROJECT" .

log_info "Deploying Smart Router Frontend UI to Cloud Run with Secret Manager mounts..."
gcloud run deploy gemini-smart-router-ui \
  --image "gcr.io/$GOOGLE_CLOUD_PROJECT/smart-router-frontend" \
  --region "$CLOUD_RUN_REGION" \
  --service-account "gemini-router-runner@$GOOGLE_CLOUD_PROJECT.iam.gserviceaccount.com" \
  --project "$GOOGLE_CLOUD_PROJECT" \
  --update-env-vars="^@^GOOGLE_CLOUD_PROJECT=${GOOGLE_CLOUD_PROJECT}@GEMINI_LOCATION=${DEPLOY_REGION}@LOCATION=${DEPLOY_REGION}@BACKEND_SERVICE_NAME=gemini-smart-router@BACKEND_API_URL=${BACKEND_API_URL}@PROXY_PUBLIC_URL=${PROXY_PUBLIC_URL}@FIREBASE_API_KEY=${FIREBASE_API_KEY}@FIREBASE_WEB_API_KEY=${FIREBASE_WEB_API_KEY}@FIREBASE_AUTH_DOMAIN=${FIREBASE_AUTH_DOMAIN}@FIREBASE_PROJECT_ID=${GOOGLE_CLOUD_PROJECT}@FIREBASE_STORAGE_BUCKET=${FIREBASE_STORAGE_BUCKET}@FIREBASE_MESSAGING_SENDER_ID=${FIREBASE_MESSAGING_SENDER_ID}@FIREBASE_APP_ID=${FIREBASE_APP_ID}@ALLOWED_EMAIL_DOMAINS=${ALLOWED_EMAIL_DOMAINS}@ALLOWED_AUTH_DOMAINS=${ALLOWED_AUTH_DOMAINS}@ALLOWED_DOMAINS=${ALLOWED_DOMAINS}@ALLOWED_EMAILS=${ALLOWED_EMAILS}" \
  --set-secrets="${SECRET_MOUNTS}" \
  --quiet

# Synchronize Firebase Identity Platform authorizedDomains non-destructively
FRONTEND_URL=$(gcloud run services describe gemini-smart-router-ui --region "$CLOUD_RUN_REGION" --format="value(status.url)" --project="$GOOGLE_CLOUD_PROJECT" 2>/dev/null || true)
AUTH_TOKEN=$(gcloud auth application-default print-access-token 2>/dev/null || gcloud auth print-access-token 2>/dev/null || true)
if [ -n "$AUTH_TOKEN" ]; then
    log_info "Synchronizing Firebase Identity Platform authorizedDomains..."
    FRONTEND_HOST="${FRONTEND_URL#https://}"; FRONTEND_HOST="${FRONTEND_HOST%/}"
    BACKEND_HOST="${BACKEND_URL#https://}"; BACKEND_HOST="${BACKEND_HOST%/}"
    PROJECT_NUM=$(gcloud projects describe "$GOOGLE_CLOUD_PROJECT" --format="value(projectNumber)" 2>/dev/null || true)
    CURRENT_IDP=$(curl -s -H "Authorization: Bearer $AUTH_TOKEN" -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
        "https://identitytoolkit.googleapis.com/admin/v2/projects/${GOOGLE_CLOUD_PROJECT}/config" || echo "{}")
    MERGED_DOMAINS=$(echo "$CURRENT_IDP" | jq -c \
        --arg proj "$GOOGLE_CLOUD_PROJECT" \
        --arg pnum "$PROJECT_NUM" \
        --arg reg "$CLOUD_RUN_REGION" \
        --arg fh "$FRONTEND_HOST" \
        --arg bh "$BACKEND_HOST" \
        --arg allowed "$ALLOWED_EMAIL_DOMAINS" '
        ((.authorizedDomains // []) +
         ["localhost", ($proj + ".firebaseapp.com"), ($proj + ".web.app"), $fh, $bh] +
         (if $pnum != "" then [("gemini-smart-router-ui-" + $pnum + "." + $reg + ".run.app"), ("gemini-smart-router-" + $pnum + "." + $reg + ".run.app")] else [] end) +
         ($allowed | split(",") | map(gsub("^\\s+|\\s+$"; "") | ltrimstr("@")) | map(select(length > 0 and (contains("@") | not))))
        ) | map(select(length > 0)) | unique
    ')
    if [ -n "$MERGED_DOMAINS" ] && [ "$MERGED_DOMAINS" != "null" ]; then
        curl -s -o /dev/null -X PATCH \
            -H "Authorization: Bearer $AUTH_TOKEN" \
            -H "x-goog-user-project: ${GOOGLE_CLOUD_PROJECT}" \
            -H "Content-Type: application/json" \
            -d "{\"authorizedDomains\": ${MERGED_DOMAINS}}" \
            "https://identitytoolkit.googleapis.com/admin/v2/projects/${GOOGLE_CLOUD_PROJECT}/config?updateMask=authorizedDomains" || true
    fi
fi

log_info "Triggering Google Cloud Build and Deploying active Go traffic generator to Cloud Run..."
gcloud run deploy gemini-traffic-generator \
  --source ./generator \
  --region "$CLOUD_RUN_REGION" \
  --service-account "gemini-router-runner@$GOOGLE_CLOUD_PROJECT.iam.gserviceaccount.com" \
  --project "$GOOGLE_CLOUD_PROJECT" \
  --update-env-vars="ROUTER_URL=${BACKEND_URL},GOOGLE_CLOUD_PROJECT=${GOOGLE_CLOUD_PROJECT}" \
  --quiet

# 6. Post-execution teardown / dangling image cleanup
log_info "Pruning untagged container images in registry and local container cache..."
for img in "gcr.io/$GOOGLE_CLOUD_PROJECT/smart-router-backend" "gcr.io/$GOOGLE_CLOUD_PROJECT/smart-router-frontend"; do
    for digest in $(gcloud container images list-tags "$img" --filter='-tags:*' --format='get(digest)' --project="$GOOGLE_CLOUD_PROJECT" 2>/dev/null || true); do
        gcloud container images delete "${img}@${digest}" --force-delete-tags --quiet --project="$GOOGLE_CLOUD_PROJECT" 2>/dev/null || true
    done
done
if command -v container >/dev/null 2>&1; then
    container image prune -f >/dev/null 2>&1 || true
fi

log_success "Deployment completed successfully!"

# Get Cloud Run service URLs
FRONTEND_URL=$(gcloud run services describe gemini-smart-router-ui --region "$CLOUD_RUN_REGION" --format="value(status.url)" --project="$GOOGLE_CLOUD_PROJECT" 2>/dev/null || true)

if [ -n "$BACKEND_URL" ] && [ -n "$FRONTEND_URL" ]; then
    echo -e "\n------------------------------------------------------------"
    echo -e "Smart Router Service URLs:"
    echo -e "👉 Backend API & Proxy: ${GREEN}$BACKEND_URL${NC}"
    echo -e "👉 Frontend Dashboard : ${GREEN}$FRONTEND_URL${NC}"
    echo -e "------------------------------------------------------------\n"

    log_info "Initiating post-deployment verification tests..."
    export SERVICE_URL="$BACKEND_URL"
    if go run cmd/verify/main.go; then
        log_success "Post-deployment verification tests passed!"
    else
        log_error "Post-deployment verification tests failed! Please check logs."
        exit 1
    fi
fi

