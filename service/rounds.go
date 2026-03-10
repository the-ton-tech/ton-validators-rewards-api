package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync/atomic"
	"time"

	"github.com/tonkeeper/tongo/ton"
	"golang.org/x/sync/errgroup"

	"github.com/the-ton-tech/ton-validators-rewards-api/model"
)

// roundInfo holds intermediate data for a single validation round.
type roundInfo struct {
	electionID     int64
	startUtime     uint32
	endUtime       uint32
	startBlock     uint32
	endBlock       uint32
	finished       bool
	fetchErr       string
	prevElectionID *int64
	nextElectionID *int64
}

// roundFetchErr returns a short, user-friendly error message for a round fetch failure.
func roundFetchErr(err error) string {
	if isPermanentError(err) {
		return "state already gc'd"
	}
	return err.Error()
}

func getAnchorExt(ctx context.Context, client LiteClient, block_seqno *uint32, election_id *int64) (*ton.BlockIDExt, error) {
	var anchorExt ton.BlockIDExt
	switch {
	case block_seqno != nil:
		ext, _, err := lookupMasterchainBlock(ctx, client, *block_seqno)
		if err != nil {
			return nil, fmt.Errorf("lookupMasterchainBlock(%d): %w", *block_seqno, err)
		}
		anchorExt = ext

	case election_id != nil:
		ext, err := lookupMasterchainBlockByUtime(ctx, client, uint32(*election_id))
		if err != nil {
			return nil, fmt.Errorf("lookupMasterchainBlockByUtime(election_id=%d): %w", *election_id, err)
		}
		anchorExt = ext
	}
	return &anchorExt, nil
}

// fetchPrevElectionIDForBlock returns the election_id of the round containing (startBlock - 1).
func fetchPrevElectionIDForBlock(ctx context.Context, client LiteClient, startBlock uint32) *int64 {
	if startBlock <= 1 {
		return nil
	}
	prevBlock := startBlock - 1
	pinnedExt, _, err := lookupMasterchainBlock(ctx, client, prevBlock)
	if err != nil {
		return nil
	}
	since, _, err := getConfigParam34(ctx, client, pinnedExt)
	if err != nil || since == 0 {
		return nil
	}
	p := int64(since)
	return &p
}

// getConfigParam34 reads config param 34 pinned to the given block and returns since/until.
func getConfigParam34(ctx context.Context, client LiteClient, ext ton.BlockIDExt) (since, until uint32, err error) {
	pinned := client.WithBlock(ext)
	conf, err := retry(func() (*ton.BlockchainConfig, error) {
		model.CountRPC(ctx)
		params, err := pinned.GetConfigParams(ctx, 0, []uint32{34})
		if err != nil {
			return nil, fmt.Errorf("GetConfigParams: %w", err)
		}
		c, _, err := ton.ConvertBlockchainConfig(params, true)
		if err != nil {
			return nil, fmt.Errorf("ConvertBlockchainConfig: %w", err)
		}
		return c, nil
	})
	if err != nil {
		return 0, 0, err
	}
	since, until = getRoundInfo(conf)
	return since, until, nil
}

// FetchRoundRewards computes per-validator and per-nominator reward distribution
// for a finished validation round using the elector's bonuses value.
func (s *Service) FetchRoundRewards(ctx context.Context, query model.RoundRewardsQuery) (*model.RoundRewardsOutput, error) {
	client := s.currentClient()

	anchor, err := getAnchorExt(ctx, client, query.Block, query.ElectionID)
	if err != nil || anchor == nil {
		return nil, fmt.Errorf("getAnchorExt error or nil: %w", err)
	}
	anchorExt := *anchor

	// Resolve round boundaries from config param 34.
	since, until, err := getConfigParam34(ctx, client, anchorExt)
	if err != nil {
		return nil, fmt.Errorf("getConfigParam34: %w", err)
	}
	if since == 0 {
		return nil, fmt.Errorf("config param 34 is empty at block %d", anchorExt.Seqno)
	}

	// Verify the round is finished.
	if time.Unix(int64(until), 0).After(time.Now()) {
		return nil, fmt.Errorf("round %d is not finished yet (ends %s)", since, time.Unix(int64(until), 0).UTC().Format(time.RFC3339))
	}

	// Resolve start_block and end_block.
	startExt, err := lookupMasterchainBlockByUtime(ctx, client, since)
	if err != nil {
		return nil, fmt.Errorf("lookupMasterchainBlockByUtime(since=%d): %w", since, err)
	}
	endExt, err := lookupMasterchainBlockByUtime(ctx, client, until)
	if err != nil {
		return nil, fmt.Errorf("lookupMasterchainBlockByUtime(until=%d): %w", until, err)
	}
	endBlock := endExt.Seqno - 1 // end_block is the last block of this round

	// Pin to end_block + 1 and fetch data in parallel.
	pinnedExt, _, err := lookupMasterchainBlock(ctx, client, endExt.Seqno)
	if err != nil {
		return nil, fmt.Errorf("lookupMasterchainBlock(end+1=%d): %w", endExt.Seqno, err)
	}
	pinned := client.WithBlock(pinnedExt)

	// fetchRoundData: parallel fetches for config, pools, and past elections at end_block+1.
	rd, err := fetchRoundData(ctx, pinned)
	if err != nil {
		return nil, err
	}

	// Build validator rows.
	rows, totalTrueStake := buildValidatorRows(rd.conf, rd.pools)
	if len(rows) == 0 {
		return nil, fmt.Errorf("no validators found in config param 34 at block %d", pinnedExt.Seqno)
	}

	if totalTrueStake.Sign() == 0 {
		return nil, fmt.Errorf("total true stake is zero — no pool data available")
	}

	// Find matching election and extract bonuses + total_stake.
	electionID := int64(since)
	el := findElection(rd.elections, electionID)
	if el == nil || el.Bonuses == nil {
		return nil, fmt.Errorf("election %d not found in past_elections or bonuses not available", electionID)
	}
	bonuses := el.Bonuses
	electionTotalStake := el.TotalStake

	validatorRewards := computeValidatorRewards(ctx, pinned, rows, totalTrueStake, bonuses)

	out := &model.RoundRewardsOutput{
		ElectionID:   electionID,
		RoundStart:   time.Unix(int64(since), 0).UTC().Format(time.RFC3339),
		RoundEnd:     time.Unix(int64(until), 0).UTC().Format(time.RFC3339),
		StartBlock:   startExt.Seqno,
		EndBlock:     endBlock,
		TotalBonuses: bonuses,
		TotalStake:   electionTotalStake,
		Validators:   validatorRewards,
	}
	out.PrevElectionID = fetchPrevElectionIDForBlock(ctx, client, startExt.Seqno)
	nextID := int64(until)
	out.NextElectionID = &nextID
	return out, nil
}

// FetchValidationRounds returns metadata for past and current validation rounds.
// It resolves the anchor round sequentially, then estimates middle blocks for
// remaining rounds and fetches them all in parallel.
func (s *Service) FetchValidationRounds(ctx context.Context, query model.RoundsQuery) (*model.ValidationRoundsOutput, error) {
	client := s.currentClient()

	// Determine anchor block.
	var anchorExt ton.BlockIDExt
	switch {
	case query.Block != nil:
		fallthrough
	case query.ElectionID != nil:
		anchor, err := getAnchorExt(ctx, client, query.Block, query.ElectionID)
		if err != nil || anchor == nil {
			return nil, fmt.Errorf("getAnchorExt error or nil: %w", err)
		}
		anchorExt = *anchor
	default:
		ext, err := retry(func() (ton.BlockIDExt, error) {
			model.CountRPC(ctx)
			res, err := client.GetMasterchainInfo(ctx)
			if err != nil {
				return ton.BlockIDExt{}, err
			}
			return ton.BlockIDExt{
				BlockID:  ton.BlockID{Workchain: int32(res.Last.Workchain), Shard: res.Last.Shard, Seqno: res.Last.Seqno},
				RootHash: ton.Bits256(res.Last.RootHash),
				FileHash: ton.Bits256(res.Last.FileHash),
			}, nil
		})
		if err != nil {
			return nil, fmt.Errorf("GetMasterchainInfo: %w", err)
		}
		anchorExt = ext
	}

	limit := 1

	now := time.Now()
	var rounds []roundInfo
	var walkErr string

	// --- Step 1: Resolve anchor round
	anchorSince, anchorUntil, err := getConfigParam34(ctx, client, anchorExt)
	if err != nil {
		walkErr = fmt.Sprintf("stopped after 0 rounds: %v", err)
		log.Printf("warning: %s", walkErr)
		return buildRoundsOutput(rounds, walkErr), nil
	}
	if anchorSince == 0 {
		return buildRoundsOutput(rounds, walkErr), nil
	}

	startExt, err := lookupMasterchainBlockByUtime(ctx, client, anchorSince)
	if err != nil {
		walkErr = fmt.Sprintf("stopped after 0 rounds: %v", err)
		log.Printf("warning: %s", walkErr)
		return buildRoundsOutput(rounds, walkErr), nil
	}

	anchor := roundInfo{
		electionID: int64(anchorSince),
		startUtime: anchorSince,
		endUtime:   anchorUntil,
		startBlock: startExt.Seqno,
	}

	// Determine if anchor round is finished and compute roundLength.
	var roundLength uint32
	if anchorUntil > 0 && time.Unix(int64(anchorUntil), 0).Before(now) {
		anchor.finished = true
		endExt, err := lookupMasterchainBlockByUtime(ctx, client, anchorUntil)
		if err != nil {
			// Use rough estimate for round length.
			log.Printf("warning: could not resolve anchor end_block: %v", err)
			if anchorExt.Seqno > startExt.Seqno {
				roundLength = anchorExt.Seqno - startExt.Seqno
			}
		} else {
			// lookupByUtime returns the first block of the next round;
			// end_block is the last block of this round.
			anchor.endBlock = endExt.Seqno - 1
			roundLength = endExt.Seqno - startExt.Seqno
		}
	} else {
		// Unfinished round — extrapolate full round length from partial data.
		partialBlocks := anchorExt.Seqno - startExt.Seqno
		elapsed := uint32(now.Unix()) - anchorSince
		fullDuration := anchorUntil - anchorSince
		if partialBlocks > 0 && elapsed > 0 && fullDuration > 0 {
			roundLength = partialBlocks * fullDuration / elapsed
		}
	}

	rounds = append(rounds, anchor)

	// --- Step 2+3: Estimate middle blocks and fan out in parallel ---
	remaining := limit - 1
	if remaining > 0 && roundLength > 0 && startExt.Seqno > 1 {
		parallelRounds := make([]roundInfo, remaining)
		g := new(errgroup.Group)

		var launched atomic.Int32
		for i := 1; i <= remaining; i++ {
			offset := roundLength/2 + uint32(i-1)*roundLength
			if offset >= startExt.Seqno {
				break
			}
			middleBlock := startExt.Seqno - offset

			g.Go(func() error {
				idx := int(launched.Add(1)) - 1

				pinnedExt, _, pinErr := lookupMasterchainBlock(ctx, client, middleBlock)
				if pinErr != nil {
					parallelRounds[idx].fetchErr = roundFetchErr(pinErr)
					return nil
				}

				since, until, confErr := getConfigParam34(ctx, client, pinnedExt)
				if confErr != nil {
					parallelRounds[idx].fetchErr = roundFetchErr(confErr)
					return nil
				}
				if since == 0 {
					parallelRounds[idx].fetchErr = "empty config param 34"
					return nil
				}

				sExt, sErr := lookupMasterchainBlockByUtime(ctx, client, since)
				if sErr != nil {
					parallelRounds[idx].fetchErr = roundFetchErr(sErr)
					return nil
				}

				parallelRounds[idx] = roundInfo{
					electionID: int64(since),
					startUtime: since,
					endUtime:   until,
					startBlock: sExt.Seqno,
					finished:   true,
				}
				return nil
			})
		}

		err = g.Wait()
		n := int(launched.Load())
		if err != nil {
			walkErr = fmt.Sprintf("stopped after %d rounds: %v", n, err)
			log.Printf("warning: %s", walkErr)
			populatePrevNextElectionIDs(ctx, client, rounds)
			return buildRoundsOutput(rounds, walkErr), nil
		}

		sort.Slice(parallelRounds[:n], func(i, j int) bool {
			return parallelRounds[i].electionID > parallelRounds[j].electionID
		})

		rounds = append(rounds, parallelRounds[:n]...)
	}

	// Derive end_block for rounds after the anchor.
	for i := 1; i < len(rounds); i++ {
		if rounds[i].fetchErr != "" || rounds[i-1].startBlock == 0 {
			continue
		}
		rounds[i].endBlock = rounds[i-1].startBlock - 1
	}

	// Trim to limit.
	if len(rounds) > limit {
		rounds = rounds[:limit]
	}

	populatePrevNextElectionIDs(ctx, client, rounds)
	return buildRoundsOutput(rounds, walkErr), nil
}

func populatePrevNextElectionIDs(ctx context.Context, client LiteClient, rounds []roundInfo) {
	for i := range rounds {
		rounds[i].prevElectionID = fetchPrevElectionIDForBlock(ctx, client, rounds[i].startBlock)

		// TODO Are we sure that end utime is always the next election id?
		if rounds[i].finished && rounds[i].endUtime > 0 {
			n := int64(rounds[i].endUtime)
			rounds[i].nextElectionID = &n
		}
	}
}

// buildRoundsOutput constructs the final output from collected rounds.
func buildRoundsOutput(rounds []roundInfo, walkErr string) *model.ValidationRoundsOutput {
	out := &model.ValidationRoundsOutput{
		Rounds: make([]model.ValidationRound, len(rounds)),
		Error:  walkErr,
	}
	for i, c := range rounds {
		vr := model.ValidationRound{
			ElectionID:     c.electionID,
			StartBlock:     c.startBlock,
			EndBlock:       c.endBlock,
			Finished:       c.finished,
			Error:          c.fetchErr,
			PrevElectionID: c.prevElectionID,
			NextElectionID: c.nextElectionID,
		}
		if c.startUtime != 0 {
			vr.Start = time.Unix(int64(c.startUtime), 0).UTC()
		}
		if c.endUtime != 0 {
			vr.End = time.Unix(int64(c.endUtime), 0).UTC()
		}
		out.Rounds[i] = vr
	}
	return out
}
