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
	FetchPerBlockRewards(ctx context.Context, seqno *uint32, unixtime *uint32, shallow bool) (*model.Output, error)
	FetchValidationRounds(ctx context.Context, query model.RoundsQuery) (*model.ValidationRoundsOutput, error)
	FetchRoundRewards(ctx context.Context, query model.RoundRewardsQuery, shallow bool) (*model.RoundRewardsOutput, error)
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
	unixtime, err := parseUnixtime(r)
	if err != nil {
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if seqno != nil && unixtime != nil {
		writeError(w, "seqno and unixtime are mutually exclusive", http.StatusBadRequest)
		return
	}
	shallow := r.URL.Query().Get("shallow") == "1"

	out, err := h.svc.FetchPerBlockRewards(ctx, seqno, unixtime, shallow)
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
	hasUnixtime := r.URL.Query().Get("unixtime") != ""

	if countTrue(hasElection, hasBlock, hasUnixtime) > 1 {
		writeError(w, "election_id, block, and unixtime are mutually exclusive", http.StatusBadRequest)
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

	if hasUnixtime {
		v, err := strconv.ParseUint(r.URL.Query().Get("unixtime"), 10, 32)
		if err != nil {
			writeError(w, fmt.Sprintf("invalid unixtime %q: %v", r.URL.Query().Get("unixtime"), err), http.StatusBadRequest)
			return
		}
		u := uint32(v)
		q.Unixtime = &u
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
	hasUnixtime := r.URL.Query().Get("unixtime") != ""

	if countTrue(hasElection, hasBlock, hasUnixtime) > 1 {
		writeError(w, "election_id, block, and unixtime are mutually exclusive", http.StatusBadRequest)
		return
	}
	if !hasElection && !hasBlock && !hasUnixtime {
		writeError(w, "one of election_id, block, or unixtime is required", http.StatusBadRequest)
		return
	}

	shallow := r.URL.Query().Get("shallow") == "1"
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

	if hasUnixtime {
		v, err := strconv.ParseUint(r.URL.Query().Get("unixtime"), 10, 32)
		if err != nil {
			writeError(w, fmt.Sprintf("invalid unixtime %q: %v", r.URL.Query().Get("unixtime"), err), http.StatusBadRequest)
			return
		}
		u := uint32(v)
		q.Unixtime = &u
	}

	out, err := h.svc.FetchRoundRewards(ctx, q, shallow)
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

// parseUnixtime extracts the optional unixtime query parameter from the request.
func parseUnixtime(r *http.Request) (*uint32, error) {
	s := r.URL.Query().Get("unixtime")
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid unixtime %q: %w", s, err)
	}
	u := uint32(v)
	return &u, nil
}

// countTrue returns how many of the given booleans are true.
func countTrue(vals ...bool) int {
	n := 0
	for _, v := range vals {
		if v {
			n++
		}
	}
	return n
}
