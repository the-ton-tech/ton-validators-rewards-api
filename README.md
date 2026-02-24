# validators-statistics

HTTP API that returns current TON validator statistics — stakes, per-block rewards, pool addresses, and nominator breakdowns — fetched directly from TON liteservers via the [tongo](https://github.com/tonkeeper/tongo) library.

## Quick start

```bash
go build -o validators-statistics .
./validators-statistics
```

The server listens on port `8080` by default. Override with the `PORT` environment variable:

```bash
PORT=3000 ./validators-statistics
```

On first launch the TON global config is downloaded and cached to `/tmp/ton-global-config.json` for 7 days. The client connects to up to 10 liteservers in parallel.

## API

### `GET /health`

Returns `{"status":"ok"}`.

### `GET /api/validators`

Returns all current validators with stakes, rewards, pool addresses, and nominators.

Query parameters:

| Parameter | Type   | Description                                      |
|-----------|--------|--------------------------------------------------|
| `seqno`   | uint32 | Masterchain block seqno (defaults to latest)     |

Response:

```json
{
  "response_time_ms": 12345,
  "block": {
    "seqno": 57486221,
    "time": "2026-02-20T13:51:37Z"
  },
  "validation_round": {
    "start": "2026-02-20T12:09:44Z",
    "end": "2026-02-21T06:22:00Z"
  },
  "election_params": {
    "validators_elected_for_sec": 65536,
    "elections_start_before_sec": 32768,
    "elections_end_before_sec": 8192,
    "stake_held_for_sec": 32768
  },
  "elector_balance": 966674188286983322,
  "total_stake": 457752122739238021,
  "reward_per_block": 2928989965,
  "validators": [
    {
      "rank": 1,
      "pubkey": "e33f0e53552f951e...",
      "stake": 2127654606060000,
      "share": 0.004648,
      "per_block_reward": 13614090,
      "pool": "Ef_bmCmMPsrHKOC4hV8foWBs2TEUAggQ1Wfe6EAqjrI3sGNI",
      "nominators": [
        {
          "address": "Ef9dcnCvPwcmBf-JbyIyY47LYCJ3obFCpRG-XhXMV1er1myc",
          "share": 1.0,
          "per_block_reward": 13614090,
          "staked": 2127654606060000,
          "pool_balance": 2376902585342169
        }
      ]
    }
  ]
}
```

### `GET /api/validators/{pubkey}`

Returns a single validator entry by hex public key.

Query parameters: same as above (`seqno`).

## Project structure

```
main.go              Entry point
model/model.go       JSON response types
service/client.go    TON lite client initialization + config caching
service/stats.go     FetchStats() — core data-fetching orchestrator
service/pool.go      Pool detection, past_elections parsing, hashmap traversal
service/blockchain.go Block lookup, round info, validator extraction
service/rpccount.go  Per-request RPC call counter
api/handler.go       HTTP handlers and router
```

Dependency graph (no cycles):

```
main → api → service → model
```

## How it works

1. Connects to TON liteservers using the global config
2. Resolves the target masterchain block (latest or by seqno)
3. Reads config params 1 (elector address), 15 (election timing), and 34 (current validators)
4. Calls the elector's `past_elections` method to get pool addresses and true stakes
5. Fetches nominator lists for each pool in parallel (up to 20 concurrent RPC calls)
6. Returns the assembled JSON response

All values (stakes, balances, rewards) are in nanoTON.
