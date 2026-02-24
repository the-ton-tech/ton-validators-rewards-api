package main

import (
	"log"
	"net/http"
	"os"

	"github.com/tonkeeper/validators-statistics/api"
	"github.com/tonkeeper/validators-statistics/service"
)

func main() {
	client, err := service.NewClientWithCachedConfig()
	if err != nil {
		log.Fatalf("client: %v", err)
	}

	mux := api.NewRouter(client)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
