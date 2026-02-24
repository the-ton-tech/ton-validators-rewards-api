package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/tonkeeper/tongo/liteapi"

	"github.com/tonkeeper/validators-statistics/service"
)

// NewRouter registers all routes and returns the mux.
func NewRouter(client *liteapi.Client) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /api/validators/{pubkey}", handleValidatorByPubkey(client))
	mux.HandleFunc("GET /api/validators", handleValidators(client))

	return mux
}

// handleValidators returns an HTTP handler for GET /api/validators.
func handleValidators(client *liteapi.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := service.WithRPCCounter(r.Context())

		seqno, err := parseSeqno(r)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}

		out, err := service.FetchStats(ctx, client, seqno)
		if err != nil {
			log.Printf("FetchStats error: %v", err)
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		elapsed := time.Since(start)
		out.ResponseTimeMs = elapsed.Milliseconds()
		log.Printf("GET /api/validators: %dms, %d RPC calls", elapsed.Milliseconds(), service.RPCCount(ctx))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}
}

// handleValidatorByPubkey returns an HTTP handler for GET /api/validators/{pubkey}.
func handleValidatorByPubkey(client *liteapi.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := service.WithRPCCounter(r.Context())
		pubkey := r.PathValue("pubkey")

		seqno, err := parseSeqno(r)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}

		out, err := service.FetchStats(ctx, client, seqno)
		if err != nil {
			log.Printf("FetchStats error: %v", err)
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for _, v := range out.Validators {
			if v.Pubkey == pubkey {
				elapsed := time.Since(start)
				v.ResponseTimeMs = elapsed.Milliseconds()
				log.Printf("GET /api/validators/%s: %dms, %d RPC calls", pubkey, elapsed.Milliseconds(), service.RPCCount(ctx))
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(v)
				return
			}
		}

		writeError(w, fmt.Sprintf("validator %q not found", pubkey), http.StatusNotFound)
	}
}

// writeError writes a structured JSON error response.
func writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// parseSeqno extracts the optional seqno query parameter from the request.
func parseSeqno(r *http.Request) (*uint32, error) {
	s := r.URL.Query().Get("seqno")
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid seqno %q: %w", s, err)
	}
	u := uint32(v)
	return &u, nil
}
