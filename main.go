package main

import (
	_ "embed"
	"log"
	"net/http"
	"os"

	"github.com/tonkeeper/validators-statistics/api"
	"github.com/tonkeeper/validators-statistics/service"
)

//go:embed openapi.yaml
var openapiSpec []byte

func main() {
	client, err := service.NewClientWithCachedConfig()
	if err != nil {
		log.Fatalf("client: %v", err)
	}

	svc := service.New(client)
	apiSvc := api.NewService(svc)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /api/validators", apiSvc.HandleValidators)

	mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})

	mux.HandleFunc("GET /swagger", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(api.SwaggerHTML))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
