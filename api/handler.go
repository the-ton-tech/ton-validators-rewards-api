package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/the-ton-tech/ton-validators-rewards-api/model"
)

// ValidatorService describes the methods the API layer needs from the service layer.
type ValidatorService interface {
	FetchPerBlockRewards(ctx context.Context, seqno *uint32, includeNominators bool) (*model.Output, error)
	FetchValidationRounds(ctx context.Context, query model.RoundsQuery) (*model.ValidationRoundsOutput, error)
	FetchRoundRewards(ctx context.Context, query model.RoundRewardsQuery) (*model.RoundRewardsOutput, error)
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

	out, err := h.svc.FetchPerBlockRewards(ctx, seqno, includeNominators)
	if err != nil {
		log.Printf("FetchPerBlockRewards error: %v", err)
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	elapsed := time.Since(start)
	out.ResponseTimeMs = elapsed.Milliseconds()
	log.Printf("GET /api/validators: %dms, %d RPC calls", elapsed.Milliseconds(), model.RPCCount(ctx))
	writeJSON(w, out)
}

// HandleValidationRounds handles GET /api/validation-rounds.
func (h *Service) HandleValidationRounds(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := model.WithRPCCounter(r.Context())

	q := model.RoundsQuery{}

	hasElection := r.URL.Query().Get("election_id") != ""
	hasBlock := r.URL.Query().Get("block") != ""

	if hasElection && hasBlock {
		writeError(w, "election_id and block are mutually exclusive", http.StatusBadRequest)
		return
	}

	if hasElection {
		v, err := strconv.ParseInt(r.URL.Query().Get("election_id"), 10, 64)
		if err != nil {
			writeError(w, fmt.Sprintf("invalid election_id %q: %v", r.URL.Query().Get("election_id"), err), http.StatusBadRequest)
			return
		}
		q.ElectionID = &v
	}

	if hasBlock {
		v, err := strconv.ParseUint(r.URL.Query().Get("block"), 10, 32)
		if err != nil {
			writeError(w, fmt.Sprintf("invalid block %q: %v", r.URL.Query().Get("block"), err), http.StatusBadRequest)
			return
		}
		u := uint32(v)
		q.Block = &u
	}

	out, err := h.svc.FetchValidationRounds(ctx, q)
	if err != nil {
		log.Printf("FetchValidationRounds error: %v", err)
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	elapsed := time.Since(start)
	out.ResponseTimeMs = elapsed.Milliseconds()
	log.Printf("GET /api/validation-rounds: %dms, %d RPC calls", elapsed.Milliseconds(), model.RPCCount(ctx))
	writeJSON(w, out)
}

// HandleRoundRewards handles GET /api/round-rewards.
func (h *Service) HandleRoundRewards(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := model.WithRPCCounter(r.Context())

	hasElection := r.URL.Query().Get("election_id") != ""
	hasBlock := r.URL.Query().Get("block") != ""

	if hasElection && hasBlock {
		writeError(w, "election_id and block are mutually exclusive", http.StatusBadRequest)
		return
	}
	if !hasElection && !hasBlock {
		writeError(w, "one of election_id or block is required", http.StatusBadRequest)
		return
	}

	var q model.RoundRewardsQuery

	if hasElection {
		v, err := strconv.ParseInt(r.URL.Query().Get("election_id"), 10, 64)
		if err != nil {
			writeError(w, fmt.Sprintf("invalid election_id %q: %v", r.URL.Query().Get("election_id"), err), http.StatusBadRequest)
			return
		}
		q.ElectionID = &v
	}

	if hasBlock {
		v, err := strconv.ParseUint(r.URL.Query().Get("block"), 10, 32)
		if err != nil {
			writeError(w, fmt.Sprintf("invalid block %q: %v", r.URL.Query().Get("block"), err), http.StatusBadRequest)
			return
		}
		u := uint32(v)
		q.Block = &u
	}

	out, err := h.svc.FetchRoundRewards(ctx, q)
	if err != nil {
		log.Printf("FetchRoundRewards error: %v", err)
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	elapsed := time.Since(start)
	out.ResponseTimeMs = elapsed.Milliseconds()
	log.Printf("GET /api/round-rewards: %dms, %d RPC calls", elapsed.Milliseconds(), model.RPCCount(ctx))
	writeJSON(w, out)
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
