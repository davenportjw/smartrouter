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

	"geminirouter/pkg/auth"
	"geminirouter/pkg/config"
	"geminirouter/pkg/dashboard"
	"geminirouter/pkg/proxy"
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
	// Metadata server returns: projects/[NUM]/regions/[REGION]
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

	ctx := context.Background()

	// Initialize Firestore Config Store
	configStore, err := config.NewConfigStore(ctx, projectID)
	if err != nil {
		log.Fatalf("Failed to initialize Firestore configuration: %v", err)
	}

	// Start Firestore real-time configuration synchronization in background
	configStore.StartListeners(ctx)

	// Initialize Firebase Auth Store
	authStore, err := auth.NewAuthStore(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize Firebase Authentication: %v", err)
	}

	// Initialize our Gemini Proxy
	geminiProxy, err := proxy.NewRouterProxy(configStore, projectID, location)
	if err != nil {
		log.Fatalf("Failed to initialize proxy: %v", err)
	}

	// Initialize Dashboard Controller
	dashController := dashboard.NewDashboardController(configStore, projectID, location)

	// Verify and bootstrap models registry on first start if empty
	modelsList, err := configStore.GetAllModels(ctx)
	if err == nil && len(modelsList) == 0 {
		log.Println("[Discovery] Models registry is empty. Invoking first-start dynamic bootstrap discovery...")
		if err := dashController.DiscoverAndCacheModels(ctx); err != nil {
			log.Printf("[Discovery] Warning: first-start dynamic bootstrap discovery failed: %v", err)
		} else {
			log.Println("[Discovery] First-start dynamic bootstrap discovery completed successfully.")
		}
	}

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

	// Admin Dashboard & HTMX Operations (wrapped in Firebase auth verification middleware)
	mux.Handle("/admin/", authStore.Middleware(http.HandlerFunc(dashController.ServeApps))) // Default route redirects/renders Apps
	mux.Handle("/admin/apps", authStore.Middleware(http.HandlerFunc(dashController.ServeApps)))
	mux.Handle("/admin/apps/new", authStore.Middleware(http.HandlerFunc(dashController.ServeAppsNewModal)))
	mux.Handle("/admin/apps/create", authStore.Middleware(http.HandlerFunc(dashController.CreateApp)))
	mux.Handle("/admin/apps/delete", authStore.Middleware(http.HandlerFunc(dashController.DeleteApp)))
	mux.Handle("/admin/keys", authStore.Middleware(http.HandlerFunc(dashController.ServeKeys)))
	mux.Handle("/admin/keys/new", authStore.Middleware(http.HandlerFunc(dashController.ServeKeysNewModal)))
	mux.Handle("/admin/keys/create", authStore.Middleware(http.HandlerFunc(dashController.CreateKey)))
	mux.Handle("/admin/keys/revoke", authStore.Middleware(http.HandlerFunc(dashController.RevokeKey)))
	mux.Handle("/admin/headers", authStore.Middleware(http.HandlerFunc(dashController.ServeHeaders)))
	mux.Handle("/admin/headers/new", authStore.Middleware(http.HandlerFunc(dashController.ServeHeadersNewModal)))
	mux.Handle("/admin/headers/create", authStore.Middleware(http.HandlerFunc(dashController.CreateHeader)))
	mux.Handle("/admin/headers/delete", authStore.Middleware(http.HandlerFunc(dashController.DeleteHeader)))
	mux.Handle("/admin/rules", authStore.Middleware(http.HandlerFunc(dashController.ServeRules)))
	mux.Handle("/admin/rules/new", authStore.Middleware(http.HandlerFunc(dashController.ServeRulesNewModal)))
	mux.Handle("/admin/rules/create", authStore.Middleware(http.HandlerFunc(dashController.CreateRule)))
	mux.Handle("/admin/rules/delete", authStore.Middleware(http.HandlerFunc(dashController.DeleteRule)))
	mux.Handle("/admin/complexity", authStore.Middleware(http.HandlerFunc(dashController.ServeComplexity)))
	mux.Handle("/admin/complexity/edit", authStore.Middleware(http.HandlerFunc(dashController.ServeComplexityEditModal)))
	mux.Handle("/admin/complexity/save", authStore.Middleware(http.HandlerFunc(dashController.SaveComplexitySettings)))
	mux.Handle("/admin/models", authStore.Middleware(http.HandlerFunc(dashController.ServeModels)))
	mux.Handle("/admin/models/refresh", authStore.Middleware(http.HandlerFunc(dashController.RefreshModels)))
	mux.Handle("/admin/models/toggle", authStore.Middleware(http.HandlerFunc(dashController.ToggleModel)))
	mux.Handle("/admin/metrics", authStore.Middleware(http.HandlerFunc(dashController.ServeMetrics)))
	mux.Handle("/admin/costs", authStore.Middleware(http.HandlerFunc(dashController.ServeCosts)))
	mux.Handle("/admin/toggle-simulation", authStore.Middleware(http.HandlerFunc(dashController.ToggleSimulation)))

	// Authentication endpoints
	mux.HandleFunc("/login", dashController.ServeLogin)
	mux.HandleFunc("/logout", authStore.Logout)
	mux.HandleFunc("/auth/session", authStore.CreateSession)

	// Static Assets
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: injectSimulationContext(mux),
	}

	// Listen for system signals to shut down gracefully
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Gemini Router starting on port %s...", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	<-stop

	log.Println("Shutting down server gracefully...")

	// Allow up to 30 seconds for active in-flight requests to complete
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exiting cleanly.")
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
