package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"geminirouter/backend/api"
	store "geminirouter/backend/config"
	"geminirouter/backend/proxy"
)

// getGCPRegion queries the local GCP metadata server to fetch the current Cloud Run/GCE region.
func getGCPRegion() string {
	client := &http.Client{Timeout: 1 * time.Second}
	req, err := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/instance/region", nil)
	if err != nil {
		return "us-central1"
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return "us-central1"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "us-central1"
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "us-central1"
	}
	val := strings.TrimSpace(string(body))
	parts := strings.Split(val, "/regions/")
	if len(parts) == 2 {
		return parts[1]
	}
	return "us-central1"
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		log.Println("[Warning] GOOGLE_CLOUD_PROJECT env variable not set. Defaulting to 'dev-project'.")
		projectID = "dev-project"
	}

	location := os.Getenv("GEMINI_LOCATION")
	if location == "" {
		if os.Getenv("LOCAL_DEV") == "true" {
			location = "us-central1"
		} else {
			location = getGCPRegion()
		}
	}

	sharedSecret := os.Getenv("BACKEND_SHARED_SECRET")
	if sharedSecret == "" && os.Getenv("LOCAL_DEV") == "true" {
		log.Println("[Warning] BACKEND_SHARED_SECRET env variable not set in Local Dev mode.")
	}

	ctx := context.Background()

	// Initialize Firestore Config Store
	configStore, err := store.NewConfigStore(ctx, projectID)
	if err != nil {
		log.Fatalf("Failed to initialize configuration store: %v", err)
	}

	// Start real-time configuration synchronization
	configStore.StartListeners(ctx)

	// Initialize our Gemini Proxy
	geminiProxy, err := proxy.NewRouterProxy(configStore, projectID, location)
	if err != nil {
		log.Fatalf("Failed to initialize proxy: %v", err)
	}

	// Initialize Backend REST API Controller
	apiController := api.NewAPIController(configStore, geminiProxy.Scheduler, sharedSecret)

	mux := http.NewServeMux()

	// Gemini API Endpoints (routed through the proxy)
	mux.Handle("/v1beta/models/", geminiProxy)
	mux.Handle("/v1/models/", geminiProxy)
	mux.Handle("/v1beta/reasoningEngines/", geminiProxy)
	mux.Handle("/v1beta1/reasoningEngines/", geminiProxy)
	mux.Handle("/v1/reasoningEngines/", geminiProxy)
	mux.Handle("/v1beta/ragCorpora/", geminiProxy)
	mux.Handle("/v1beta1/ragCorpora/", geminiProxy)
	mux.Handle("/v1/ragCorpora/", geminiProxy)

	// Administrative REST API routes
	apiController.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: injectSimulationContext(mux),
	}

	// Listen for system signals to shut down gracefully
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Smart Router Backend Service starting on port %s...", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	<-stop

	log.Println("Shutting down backend server gracefully...")

	// Allow up to 30 seconds for active in-flight requests to complete
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Backend server exiting cleanly.")
}

// injectSimulationContext extracts the simulate_metrics cookie and injects its value into the request context.
func injectSimulationContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		simulate := false
		if cookie, err := r.Cookie("simulate_metrics"); err == nil && cookie.Value == "true" {
			simulate = true
		}
		ctx := context.WithValue(r.Context(), "simulate_metrics", simulate)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
