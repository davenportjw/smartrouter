package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"geminirouter/pkg/runner/engine"
)

func main() {
	// 1. Parse deployment flags
	routerURL := flag.String("router-url", "http://localhost:8080", "Smart Router Control Plane URL")
	cpuMode := flag.Bool("cpu-mode", false, "Enable CPU-only inference mode to keep hardware costs down")
	clusterID := flag.String("cluster-id", "", "The target cluster pool ID to join")
	modelListStr := flag.String("supported-models", "", "Comma-separated list of supported model tags")
	nodeName := flag.String("node-name", "", "Human-readable name for this host node")
	flag.Parse()

	if *routerURL == "" {
		log.Fatal("Error: --router-url flag is mandatory")
	}

	// 2. Resolve defaults based on CPU / GPU mode selection
	var models []string
	if *modelListStr != "" {
		models = strings.Split(*modelListStr, ",")
	} else {
		if *cpuMode {
			models = []string{"gemma2:2b", "gemma2:2b-instruct-q4_K_M"} // Light CPU-friendly models
		} else {
			models = []string{"gemma2:9b", "llama3:8b", "llama3:70b"} // Heavy GPU models
		}
	}

	resolvedClusterID := *clusterID
	if resolvedClusterID == "" {
		if *cpuMode {
			resolvedClusterID = "gke-cpu-spot-pool"
		} else {
			resolvedClusterID = "gke-gpu-spot-pool"
		}
	}

	resolvedNodeName := *nodeName
	if resolvedNodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			resolvedNodeName = "unknown-runner"
		} else {
			resolvedNodeName = hostname
		}
	}

	log.Printf("======================================================================")
	log.Printf("       S M A R T   R O U T E R   -   C O M P U T E   R U N N E R       ")
	log.Printf("======================================================================")
	log.Printf("👉 Runner Name  : %s", resolvedNodeName)
	log.Printf("👉 Router URL   : %s", *routerURL)
	log.Printf("👉 Cluster ID   : %s", resolvedClusterID)
	log.Printf("👉 Mode Type    : %s", func() string {
		if *cpuMode {
			return "🔵 CPU Inference Only (Cost-Saver)"
		}
		return "🟢 GPU Accelerated Inference"
	}())
	log.Printf("👉 Serving Models: %s", strings.Join(models, ", "))
	log.Printf("======================================================================")

	// 3. Instantiate and initialize the Core Runner Engine
	runner := engine.NewCoreRunnerEngine(*routerURL, models)
	runner.ClusterID = resolvedClusterID
	
	// Customize node parameters depending on CPU / GPU profiles
	runner.NodeID = fmt.Sprintf("runner-%s-%d", resolvedNodeName, time.Now().UnixNano()%10000)
	
	// Profile system memory dynamically (simulated baseline based on mode flags)
	if *cpuMode {
		runner.MemoryAllocatedGB = 8 // Cost-effective 8GB CPU pod slice
		runner.ComputeGPUCores = 0   // No GPU accelerators requested
	} else {
		runner.MemoryAllocatedGB = 24 // High performance 24GB GPU slice (Nvidia L4 standard)
		runner.ComputeGPUCores = 2560 // Simulated CUDA/Tensor acceleration index
	}

	// 4. Start Execution Loop
	go runner.StartLoop()

	// Handle OS shutdown signals gracefully
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down compute runner engine gracefully...")
	runner.Stop()
	log.Println("Runner exited cleanly.")
}
