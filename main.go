package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"geminirouter/pkg/auth"
	"geminirouter/pkg/config"
	"geminirouter/pkg/dashboard"
	"geminirouter/pkg/proxy"
)

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
		location = "us-central1"
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
	mux.Handle("/admin/metrics", authStore.Middleware(http.HandlerFunc(dashController.ServeMetrics)))
	mux.Handle("/admin/costs", authStore.Middleware(http.HandlerFunc(dashController.ServeCosts)))

	// Authentication endpoints
	mux.HandleFunc("/login", dashController.ServeLogin)
	mux.HandleFunc("/logout", authStore.Logout)
	mux.HandleFunc("/auth/session", authStore.CreateSession)

	// Static Assets
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
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
