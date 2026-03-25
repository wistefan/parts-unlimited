// Package main is the entrypoint for the orchestrator service.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/wistefan/dev-env/orchestrator/pkg/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	cfg, err := config.LoadFromFile(*configPath)
	if err != nil {
		// Fall back to defaults if config file not found
		if os.IsNotExist(err) {
			log.Println("Config file not found, using defaults")
			cfg = config.DefaultConfig()
		} else {
			log.Fatalf("Failed to load config: %v", err)
		}
	}

	fmt.Printf("Orchestrator starting...\n")
	fmt.Printf("  Gitea:  %s\n", cfg.Gitea.URL)
	fmt.Printf("  Taiga:  %s\n", cfg.Taiga.URL)
	fmt.Printf("  Max concurrency: %d\n", cfg.Agents.MaxConcurrency)
	fmt.Printf("  Namespace: %s\n", cfg.Kubernetes.Namespace)

	// TODO: Initialize subsystems (webhook listener, assignment engine, lifecycle manager, etc.)
	// For now, this is a scaffold — subsystems will be added in subsequent steps.

	log.Println("Orchestrator scaffold ready. Subsystems not yet implemented.")
	select {} // Block forever (will be replaced with proper server lifecycle)
}
