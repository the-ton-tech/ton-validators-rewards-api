package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	dataloader "github.com/graph-gophers/dataloader/v7"
	"github.com/tonkeeper/tongo/liteapi"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"

	"github.com/the-ton-tech/ton-validators-rewards-api/model"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Cache implements the dataloader.Cache interface
type cache[K comparable, V any] struct {
	*lru.Cache[K, V]
}

// Get gets an item from the cache
func (c *cache[K, V]) Get(_ context.Context, key K) (V, bool) {
	v, ok := c.Cache.Get(key)
	if ok {
		return v, ok
	}
	return v, ok
}

// Set sets an item in the cache
func (c *cache[K, V]) Set(_ context.Context, key K, value V) {
	c.Cache.Add(key, value)
}

// Delete deletes an item in the cache
func (c *cache[K, V]) Delete(_ context.Context, key K) bool {
	if c.Cache.Contains(key) {
		c.Cache.Remove(key)
		return true
	}
	return false
}

// Clear clears the cache
func (c *cache[K, V]) Clear() {
	c.Cache.Purge()
}

type loadersKey struct{}

// blockResult holds the cached result of a lookupMasterchainBlock call.
type blockResult struct {
	ext  ton.BlockIDExt
	time time.Time
}

// configParamsKey is the cache key for GetConfigParams calls.
type configParamsKey struct {
	mode   liteapi.ConfigMode
	params string // fmt.Sprint of []uint32 — slices aren't comparable
	seqno  uint32
}

func newConfigParamsKey(mode liteapi.ConfigMode, paramList []uint32, seqno uint32) configParamsKey {
	return configParamsKey{mode: mode, params: fmt.Sprint(paramList), seqno: seqno}
}

// configParamsArgs stores the non-comparable arguments needed by the config batch function.
type configParamsArgs struct {
	paramList    []uint32
	pinnedClient LiteClient
}

// loaders holds per-request dataloaders.
type loaders struct {
	blockByUtime *dataloader.Loader[uint32, ton.BlockIDExt]
	blockBySeqno *dataloader.Loader[uint32, blockResult]
	configParams *dataloader.Loader[configParamsKey, tlb.ConfigParams]

	// configArgs maps configParamsKey → configParamsArgs so the batch function
	// can recover the original []uint32 and BlockIDExt from the comparable key.
	configArgsMu sync.Mutex
	configArgs   map[configParamsKey]configParamsArgs
}

var loadersMu sync.Mutex

var globalLoaders *loaders

// WithLoaders creates a new context with dataloaders backed by the given client.
func WithLoaders(ctx context.Context, client LiteClient) context.Context {
	loadersMu.Lock()
	defer loadersMu.Unlock()
	if globalLoaders != nil {
		return context.WithValue(ctx, loadersKey{}, globalLoaders)
	}

	blockSeqnoLru, err := lru.New[uint32, dataloader.Thunk[ton.BlockIDExt]](1000)
	if err != nil {
		panic(err)
	}
	blockSeqnoCache := &cache[uint32, dataloader.Thunk[ton.BlockIDExt]]{Cache: blockSeqnoLru}

	blockUtimeLru, err := lru.New[uint32, dataloader.Thunk[blockResult]](1000)
	if err != nil {
		panic(err)
	}
	blockUtimeCache := &cache[uint32, dataloader.Thunk[blockResult]]{Cache: blockUtimeLru}

	configParamsLru, err := lru.New[configParamsKey, dataloader.Thunk[tlb.ConfigParams]](1000)
	if err != nil {
		panic(err)
	}
	configParamsCache := &cache[configParamsKey, dataloader.Thunk[tlb.ConfigParams]]{Cache: configParamsLru}

	utimeBatch := func(ctx context.Context, keys []uint32) []*dataloader.Result[ton.BlockIDExt] {
		results := make([]*dataloader.Result[ton.BlockIDExt], len(keys))
		var wg sync.WaitGroup
		wg.Add(len(keys))
		for i, utime := range keys {
			go func() {
				defer wg.Done()
				blockID := ton.BlockID{
					Workchain: -1,
					Shard:     0x8000000000000000,
				}
				innerCtx, _ := WithRetryExclude(ctx)
				ext, err := retryWithExclude(innerCtx, func() (ton.BlockIDExt, error) {
					model.CountRPC(ctx)
					ext, _, err := client.LookupBlock(ctx, blockID, 4, nil, &utime)
					if err != nil {
						return ton.BlockIDExt{}, err
					}
					return ext, nil
				})
				if err != nil {
					results[i] = &dataloader.Result[ton.BlockIDExt]{Error: dataloader.NewSkipCacheError(fmt.Errorf("lookupMasterchainBlockByUtime(%d): %w", utime, err))}
				} else {
					results[i] = &dataloader.Result[ton.BlockIDExt]{Data: ext}
				}
			}()
		}
		wg.Wait()
		return results
	}

	seqnoBatch := func(ctx context.Context, keys []uint32) []*dataloader.Result[blockResult] {
		results := make([]*dataloader.Result[blockResult], len(keys))
		var wg sync.WaitGroup
		wg.Add(len(keys))
		for i, seqno := range keys {
			go func() {
				defer wg.Done()
				blockID := ton.BlockID{
					Workchain: -1,
					Shard:     0x8000000000000000,
					Seqno:     seqno,
				}
				innerCtx, _ := WithRetryExclude(ctx)
				r, err := retryWithExclude(innerCtx, func() (blockResult, error) {
					model.CountRPC(ctx)
					ext, info, err := client.LookupBlock(ctx, blockID, 1, nil, nil)
					if err != nil {
						return blockResult{}, err
					}
					return blockResult{ext, time.Unix(int64(info.GenUtime), 0)}, nil
				})
				if err != nil {
					results[i] = &dataloader.Result[blockResult]{Error: dataloader.NewSkipCacheError(fmt.Errorf("lookupMasterchainBlock(%d): %w", seqno, err))}
				} else {
					results[i] = &dataloader.Result[blockResult]{Data: r}
				}
			}()
		}
		wg.Wait()
		return results
	}

	l := &loaders{
		blockByUtime: dataloader.NewBatchedLoader(utimeBatch,
			dataloader.WithInputCapacity[uint32, ton.BlockIDExt](1),
			dataloader.WithCache(blockSeqnoCache),
		),
		blockBySeqno: dataloader.NewBatchedLoader(seqnoBatch,
			dataloader.WithInputCapacity[uint32, blockResult](1),
			dataloader.WithCache(blockUtimeCache),
		),
		configArgs: make(map[configParamsKey]configParamsArgs),
	}

	configBatch := func(ctx context.Context, keys []configParamsKey) []*dataloader.Result[tlb.ConfigParams] {
		results := make([]*dataloader.Result[tlb.ConfigParams], len(keys))
		var wg sync.WaitGroup
		wg.Add(len(keys))
		for i, key := range keys {
			go func() {
				defer wg.Done()
				l.configArgsMu.Lock()
				args := l.configArgs[key]
				l.configArgsMu.Unlock()

				params, err := retry(func() (tlb.ConfigParams, error) {
					model.CountRPC(ctx)
					return args.pinnedClient.GetConfigParams(ctx, key.mode, args.paramList)
				})
				if err != nil {
					results[i] = &dataloader.Result[tlb.ConfigParams]{Error: dataloader.NewSkipCacheError(fmt.Errorf("GetConfigParams(%d, %s): %w", key.seqno, key.params, err))}
				} else {
					results[i] = &dataloader.Result[tlb.ConfigParams]{Data: params}
				}
			}()
		}
		wg.Wait()
		return results
	}
	l.configParams = dataloader.NewBatchedLoader(
		configBatch,
		dataloader.WithInputCapacity[configParamsKey, tlb.ConfigParams](1),
		dataloader.WithCache(configParamsCache),
	)
	globalLoaders = l
	return context.WithValue(ctx, loadersKey{}, l)
}

func getLoaders(ctx context.Context) *loaders {
	l, _ := ctx.Value(loadersKey{}).(*loaders)
	return l
}

// cachedGetConfigParams fetches config params using the dataloader when available.
// pinnedClient should already be pinned to a block; seqno is used as the cache key.
func cachedGetConfigParams(ctx context.Context, pinnedClient LiteClient, mode liteapi.ConfigMode, paramList []uint32, seqno uint32) (tlb.ConfigParams, error) {
	if l := getLoaders(ctx); l != nil {
		key := newConfigParamsKey(mode, paramList, seqno)
		l.configArgsMu.Lock()
		l.configArgs[key] = configParamsArgs{paramList: paramList, pinnedClient: pinnedClient}
		l.configArgsMu.Unlock()
		thunk := l.configParams.Load(ctx, key)
		return thunk()
	}

	// Fallback: no loader in context.
	return retry(func() (tlb.ConfigParams, error) {
		model.CountRPC(ctx)
		return pinnedClient.GetConfigParams(ctx, mode, paramList)
	})
}

// lookupMasterchainBlockByUtime resolves a unix timestamp to the nearest masterchain block.
// Uses the per-request dataloader when available, falling back to a direct RPC call.
func lookupMasterchainBlockByUtime(ctx context.Context, client LiteClient, utime uint32) (ton.BlockIDExt, error) {
	if l := getLoaders(ctx); l != nil {
		thunk := l.blockByUtime.Load(ctx, utime)
		return thunk()
	}

	// Fallback: no loader in context.
	blockID := ton.BlockID{
		Workchain: -1,
		Shard:     0x8000000000000000,
	}
	ctx, _ = WithRetryExclude(ctx)
	return retryWithExclude(ctx, func() (ton.BlockIDExt, error) {
		model.CountRPC(ctx)
		ext, _, err := client.LookupBlock(ctx, blockID, 4, nil, &utime)
		if err != nil {
			return ton.BlockIDExt{}, err
		}
		return ext, nil
	})
}

// lookupMasterchainBlock resolves a seqno to a BlockIDExt and returns the block time.
// Uses the per-request dataloader when available, falling back to a direct RPC call.
func lookupMasterchainBlock(ctx context.Context, client LiteClient, seqno uint32) (ton.BlockIDExt, time.Time, error) {
	if l := getLoaders(ctx); l != nil {
		thunk := l.blockBySeqno.Load(ctx, seqno)
		r, err := thunk()
		return r.ext, r.time, err
	}

	// Fallback: no loader in context.
	blockID := ton.BlockID{
		Workchain: -1,
		Shard:     0x8000000000000000,
		Seqno:     seqno,
	}
	ctx, _ = WithRetryExclude(ctx)
	r, err := retryWithExclude(ctx, func() (blockResult, error) {
		model.CountRPC(ctx)
		ext, info, err := client.LookupBlock(ctx, blockID, 1, nil, nil)
		if err != nil {
			return blockResult{}, err
		}
		return blockResult{ext, time.Unix(int64(info.GenUtime), 0)}, nil
	})
	return r.ext, r.time, err
}
