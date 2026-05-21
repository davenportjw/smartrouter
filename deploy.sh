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

# Check, enable, and retrieve Firebase configuration automatically
setup_firebase_config() {
    # Detect if Firebase SDK variables are missing or placeholder
    if [ -z "$FIREBASE_API_KEY" ] || [ "$FIREBASE_API_KEY" = "AIzaSyYourFirebaseWebApiKey" ] || \
       [ -z "$FIREBASE_APP_ID" ] || [ "$FIREBASE_APP_ID" = "1:1234:web:abcd" ]; then
        log_info "Firebase credentials missing or default in .env. Initiating auto-configuration..."
        
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
            "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}")

        if [ "$check_code" -ne 200 ]; then
            log_info "Firebase not active on this project. Enabling Firebase..."
            local enable_code
            enable_code=$(curl -s -w "%{http_code}" -o /tmp/fb_add_resp.json \
                -X POST -H "Authorization: Bearer $auth_token" \
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
                -H "Content-Type: application/json" \
                -d '{"displayName": "Gemini Router Admin"}' \
                "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}/webApps")

            if [ "$register_code" -eq 200 ] || [ "$register_code" -eq 202 ]; then
                sleep 3
                apps_list=$(curl -s -H "Authorization: Bearer $auth_token" \
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
            "https://firebase.googleapis.com/v1beta1/projects/${GOOGLE_CLOUD_PROJECT}/webApps/${app_id}/config")

        local api_key=$(echo "$config_data" | jq -r '.apiKey // empty')
        local auth_domain=$(echo "$config_data" | jq -r '.authDomain // empty')
        local storage_bucket=$(echo "$config_data" | jq -r '.storageBucket // empty')

        if [ -z "$api_key" ] || [ "$api_key" = "null" ]; then
            log_error "No API key linked with your Web App. Please verify Credentials API configurations in GCP console."
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
        update_env_var "FIREBASE_AUTH_DOMAIN" "$auth_domain"
        update_env_var "FIREBASE_PROJECT_ID" "$GOOGLE_CLOUD_PROJECT"
        update_env_var "FIREBASE_STORAGE_BUCKET" "$storage_bucket"
        update_env_var "FIREBASE_MESSAGING_SENDER_ID" "$project_num"
        update_env_var "FIREBASE_APP_ID" "$app_id"

        log_success "Firebase credentials successfully updated automatically!"
    else
        log_info "Firebase credentials already configured in .env. Skipping automatic setup."
    fi
}

# 1. Load environmental variables
if [ ! -f .env ]; then
    log_error ".env file not found in root directory. Please copy .env.sample to .env and fill it out."
    exit 1
fi

log_info "Loading environment variables from .env..."
# Read env file ignoring comments and empty lines
export $(grep -v '^#' .env | xargs)

# Validate critical baseline variables
if [ -z "$GOOGLE_CLOUD_PROJECT" ]; then
    log_error "GOOGLE_CLOUD_PROJECT is missing in .env"
    exit 1
fi

# Invoke automated Firebase configuration
setup_firebase_config

# Generate secure BACKEND_SHARED_SECRET if it is missing
if [ -z "$BACKEND_SHARED_SECRET" ]; then
    log_info "Generating a secure BACKEND_SHARED_SECRET..."
    RAND_SECRET=$(LC_ALL=C tr -dc 'a-zA-Z0-9' < /dev/urandom | fold -w 32 | head -n 1)
    update_env_var "BACKEND_SHARED_SECRET" "$RAND_SECRET"
fi

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
  -var="firebase_api_key=$FIREBASE_API_KEY" \
  -var="firebase_auth_domain=$FIREBASE_AUTH_DOMAIN" \
  -var="firebase_storage_bucket=$FIREBASE_STORAGE_BUCKET" \
  -var="firebase_messaging_sender_id=$FIREBASE_MESSAGING_SENDER_ID" \
  -var="firebase_app_id=$FIREBASE_APP_ID" \
  -var="allowed_email_domains=$ALLOWED_EMAIL_DOMAINS" \
  -var="backend_shared_secret=$BACKEND_SHARED_SECRET"

cd ..

# 3. Dynamic Upstream authentication is governed by ADC. Skipping Secret Manager upload.

# 4. Generate Templ Templates
log_info "Compiling Go HTML Templ components..."
go run github.com/a-h/templ/cmd/templ generate

# 5. Build containers on Cloud Build and Deploy to Cloud Run
log_info "Building Smart Router Backend Container via Cloud Build..."
gcloud builds submit --config cloudbuild-backend.yaml --project "$GOOGLE_CLOUD_PROJECT" .

log_info "Deploying Smart Router Backend to Cloud Run..."
gcloud run deploy gemini-smart-router --image "gcr.io/$GOOGLE_CLOUD_PROJECT/smart-router-backend" --region us-central1 --service-account "gemini-router-runner@$GOOGLE_CLOUD_PROJECT.iam.gserviceaccount.com" --project "$GOOGLE_CLOUD_PROJECT" --update-env-vars="BACKEND_SHARED_SECRET=$BACKEND_SHARED_SECRET" --quiet

log_info "Building Smart Router Frontend UI Container via Cloud Build..."
gcloud builds submit --config cloudbuild-frontend.yaml --project "$GOOGLE_CLOUD_PROJECT" .

log_info "Deploying Smart Router Frontend UI to Cloud Run..."
gcloud run deploy gemini-smart-router-ui --image "gcr.io/$GOOGLE_CLOUD_PROJECT/smart-router-frontend" --region us-central1 --service-account "gemini-router-runner@$GOOGLE_CLOUD_PROJECT.iam.gserviceaccount.com" --project "$GOOGLE_CLOUD_PROJECT" --update-env-vars="BACKEND_SHARED_SECRET=$BACKEND_SHARED_SECRET" --quiet

log_info "Triggering Google Cloud Build and Deploying active Go traffic generator to Cloud Run..."
gcloud run deploy gemini-traffic-generator --source ./generator --region us-central1 --service-account "gemini-router-runner@$GOOGLE_CLOUD_PROJECT.iam.gserviceaccount.com" --project "$GOOGLE_CLOUD_PROJECT" --quiet

log_success "Deployment completed successfully!"

# Get Cloud Run service URLs
BACKEND_URL=$(gcloud run services describe gemini-smart-router --region us-central1 --format="value(status.url)" --project="$GOOGLE_CLOUD_PROJECT" 2>/dev/null || true)
FRONTEND_URL=$(gcloud run services describe gemini-smart-router-ui --region us-central1 --format="value(status.url)" --project="$GOOGLE_CLOUD_PROJECT" 2>/dev/null || true)

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
