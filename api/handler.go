package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/tonkeeper/validators-statistics/model"
)

// ValidatorService describes the methods the API layer needs from the service layer.
type ValidatorService interface {
	FetchStats(ctx context.Context, seqno *uint32, includeNominators bool) (*model.Output, error)
}

// Handler holds dependencies for HTTP handlers.
type Service struct {
	svc ValidatorService
}

// NewHandler creates a Handler with the given service.
func NewService(svc ValidatorService) *Service {
	return &Service{svc: svc}
}

// handleValidators handles GET /api/validators.
func (h *Service) HandleValidators(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := model.WithRPCCounter(r.Context())

	seqno, err := parseSeqno(r)
	if err != nil {
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}

	includeNominators := r.URL.Query().Get("nominators") != "false"

	out, err := h.svc.FetchStats(ctx, seqno, includeNominators)
	if err != nil {
		log.Printf("FetchStats error: %v", err)
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	elapsed := time.Since(start)
	out.ResponseTimeMs = elapsed.Milliseconds()
	log.Printf("GET /api/validators: %dms, %d RPC calls", elapsed.Milliseconds(), model.RPCCount(ctx))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// writeError writes a structured JSON error response.
func writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
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
