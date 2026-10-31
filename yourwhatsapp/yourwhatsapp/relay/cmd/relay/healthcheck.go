package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

const healthcheckTimeout = 3 * time.Second

// runHealthcheck is the `relay healthcheck` subcommand, used by the Docker
// HEALTHCHECK because the distroless image has no shell or curl. It probes the
// local listener on RELAY_PORT and returns the process exit code.
func runHealthcheck() int {
	port := os.Getenv("RELAY_PORT")
	if port == "" {
		port = "8800"
	}
	if err := healthcheck("http://127.0.0.1:" + port); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	return 0
}

func healthcheck(baseURL string) error {
	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
