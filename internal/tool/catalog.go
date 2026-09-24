package tool

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

type CatalogSnapshot struct {
	Digest  string
	BuiltAt time.Time
	Ordered []Definition
	ByName  map[string]Route
}

type CatalogCache struct {
	repo  Repository
	now   func() time.Time
	value atomic.Pointer[CatalogSnapshot]
	mu    sync.Mutex
	epoch uint64
	group singleflight.Group
}

func NewCatalog(repo Repository) *CatalogCache {
	return &CatalogCache{repo: repo, now: time.Now}
}

func (c *CatalogCache) WithClock(now func() time.Time) *CatalogCache {
	c.now = now
	return c
}

func (c *CatalogCache) Invalidate() {
	c.mu.Lock()
	c.epoch++
	c.value.Store(nil)
	c.mu.Unlock()
}

func (c *CatalogCache) Snapshot(ctx context.Context) (*CatalogSnapshot, error) {
	if snapshot := c.value.Load(); snapshot != nil {
		return snapshot, nil
	}
	result, err, _ := c.group.Do("catalog", func() (any, error) {
		for {
			if snapshot := c.value.Load(); snapshot != nil {
				return snapshot, nil
			}
			c.mu.Lock()
			epoch := c.epoch
			c.mu.Unlock()

			definitions, err := c.repo.ListAggregated(ctx)
			if err != nil {
				return nil, fmt.Errorf("load tool catalog: %w", err)
			}
			snapshot, err := buildCatalog(definitions, c.now())
			if err != nil {
				return nil, err
			}

			c.mu.Lock()
			if epoch != c.epoch {
				c.mu.Unlock()
				continue
			}
			if current := c.value.Load(); current != nil {
				c.mu.Unlock()
				return current, nil
			}
			c.value.Store(snapshot)
			c.mu.Unlock()
			return snapshot, nil
		}
	})
	if err != nil {
		return nil, err
	}
	return result.(*CatalogSnapshot), nil
}

func (c *CatalogCache) Resolve(ctx context.Context, publicName string) (Route, error) {
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		return Route{}, err
	}
	route, ok := snapshot.ByName[publicName]
	if !ok {
		return Route{}, ErrToolNotFound
	}
	return route, nil
}

func buildCatalog(definitions []Definition, builtAt time.Time) (*CatalogSnapshot, error) {
	ordered := append([]Definition(nil), definitions...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PublicName < ordered[j].PublicName })
	byName := make(map[string]Route, len(ordered))
	for _, definition := range ordered {
		if _, exists := byName[definition.PublicName]; exists {
			return nil, fmt.Errorf("%w: %q", ErrCatalogConflict, definition.PublicName)
		}
		byName[definition.PublicName] = Route{
			ServerID: definition.ServerID, ServerName: definition.ServerName,
			BackendName: definition.BackendName, PublicName: definition.PublicName,
			SnapshotID: definition.SnapshotID,
		}
	}
	return &CatalogSnapshot{
		Digest: catalogDigest(ordered), BuiltAt: builtAt.UTC(), Ordered: ordered, ByName: byName,
	}, nil
}
