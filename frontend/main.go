package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"geminirouter/frontend/auth"
	"geminirouter/frontend/dashboard"
	"geminirouter/pkg/config"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081" // Frontend defaults to 8081 locally to avoid conflict
	}

	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		log.Println("[Warning] GOOGLE_CLOUD_PROJECT env variable not set. Defaulting to 'dev-project'.")
		projectID = "dev-project"
	}

	backendAPIURL := os.Getenv("BACKEND_API_URL")
	if backendAPIURL == "" {
		backendAPIURL = "http://localhost:8080" // Defaults to local backend port
	}

	sharedSecret := os.Getenv("BACKEND_SHARED_SECRET")

	location := os.Getenv("GEMINI_LOCATION")
	if location == "" {
		location = "us-central1"
	}

	ctx := context.Background()

	// Initialize REST-based Config Client Store
	apiConfigStore := config.NewAPIConfigStore(backendAPIURL, sharedSecret)

	// Initialize Firebase Auth Store
	authStore, err := auth.NewAuthStore(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize Firebase Authentication: %v", err)
	}

	// Initialize Dashboard Controller (wrapped around our REST Client Store)
	dashController := dashboard.NewDashboardController(apiConfigStore, projectID, location)

	mux := http.NewServeMux()

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

	// Static Assets (Served locally or staged from compiled assets folder)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: injectSimulationContext(mux),
	}

	// Listen for system signals to shut down gracefully
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Smart Router Frontend Dashboard Service starting on port %s...", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start frontend server: %v", err)
		}
	}()

	<-stop

	log.Println("Shutting down frontend server gracefully...")

	// Allow up to 30 seconds for active in-flight requests to complete
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Frontend server forced to shutdown: %v", err)
	}

	log.Println("Frontend server exiting cleanly.")
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
