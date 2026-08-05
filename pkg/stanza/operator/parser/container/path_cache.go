// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package container // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/parser/container"

import (
	"sync"
)

const defaultCacheSize = 4096

type pathCacheEntry struct {
	path   string
	values map[string]any
}

type pathCache struct {
	mu    sync.Mutex
	slots []pathCacheEntry
	index int
	keys  map[string]int
}

func NewPathCache(size int) *pathCache {
	if size <= 0 {
		size = defaultCacheSize
	}
	return &pathCache{
		slots: make([]pathCacheEntry, size),
		keys:  make(map[string]int, size),
	}
}

func (c *pathCache) get(path string) (map[string]any, bool) {
	c.mu.Lock()
	i, ok := c.keys[path]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	v := c.slots[i].values
	c.mu.Unlock()
	return v, true
}

func (c *pathCache) add(path string, values map[string]any) {
	c.mu.Lock()
	// evict old occupant of this slot
	if old := c.slots[c.index].path; old != "" {
		delete(c.keys, old)
	}
	c.slots[c.index] = pathCacheEntry{path: path, values: values}
	c.keys[path] = c.index
	c.index = (c.index + 1) % len(c.slots)
	c.mu.Unlock()
}
