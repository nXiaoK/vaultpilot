package main

import (
	"net"
	"net/http"
	"os"
	"time"
)

func healthcheckURL(addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/status", nil
}

func main() {
	url, err := healthcheckURL(os.Getenv("VP_ADDR"))
	if err != nil {
		os.Exit(1)
	}
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		os.Exit(1)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		os.Exit(1)
	}
}
