package main

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if caFile := os.Getenv("HEALTHCHECK_CA_FILE"); caFile != "" {
		ca, err := os.ReadFile(caFile)
		if err != nil {
			os.Exit(1)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			os.Exit(1)
		}
		tlsConfig.RootCAs = roots
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	response, err := client.Get(os.Args[1])
	if err != nil {
		os.Exit(1)
	}
	defer response.Body.Close()
	if strings.EqualFold(response.Request.URL.Scheme, "https") && (response.TLS == nil || response.TLS.Version != tls.VersionTLS13) {
		os.Exit(1)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		os.Exit(1)
	}
}
