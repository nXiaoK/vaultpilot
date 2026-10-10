package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Get("http://127.0.0.1:8080/api/status")
	if err != nil {
		os.Exit(1)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		os.Exit(1)
	}
}
