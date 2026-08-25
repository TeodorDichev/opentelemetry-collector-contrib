// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package container // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/parser/container"

import (
	"errors"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"
	"go.opentelemetry.io/collector/component"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/entry"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/fileconsumer/attrs"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/internal/metadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/helper"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/transformer/recombine"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/stanzaerrors"
)

const (
	operatorType              = "container"
	recombineSourceIdentifier = attrs.LogFilePath
	recombineIsLastEntry      = "attributes.logtag == 'F'"
	defaultMaxLogSize         = 1024 * 1024
	defaultPathCacheSize      = 1024

	// CacheTypeNone disables the k8s metadata path cache — the path is re-parsed on every log line.
	CacheTypeNone = "none"
	// CacheTypeSyncMap uses helper.SyncMapCache (sync.Map + FIFO eviction) — lock-free concurrent reads (default).
	// This is the cache implementation from PR #44487.
	CacheTypeSyncMap = "syncmap"
	// CacheTypeLRU uses hashicorp/golang-lru/v2 for the path cache — bounded LRU eviction.
	CacheTypeLRU = "lru"
)

func init() {
	operator.Register(operatorType, func() operator.Builder { return NewConfig() })
}

// NewConfig creates a new JSON parser config with default values
func NewConfig() *Config {
	return NewConfigWithID(operatorType)
}

// NewConfigWithID creates a new JSON parser config with default values
func NewConfigWithID(operatorID string) *Config {
	return &Config{
		ParserConfig:            helper.NewParserConfig(operatorID, operatorType),
		Format:                  "",
		AddMetadataFromFilePath: true,
		MaxLogSize:              defaultMaxLogSize,
	}
}

// Config is the configuration of a Container parser operator.
type Config struct {
	helper.ParserConfig `mapstructure:",squash"`

	Format                  string          `mapstructure:"format"`
	AddMetadataFromFilePath bool            `mapstructure:"add_metadata_from_filepath"`
	MaxLogSize              helper.ByteSize `mapstructure:"max_log_size,omitempty"`

	// FilepathCacheType selects the cache used to store parsed k8s metadata
	// (namespace, pod name, uid, container name, restart count) keyed by log file path.
	// The cache avoids re-parsing the path on every log line; one entry per unique path.
	//   - "" or "syncmap" (default): helper.SyncMapCache — sync.Map + FIFO eviction (from PR #44487)
	//   - "lru": LRU eviction via hashicorp/golang-lru — bounded memory under high pod churn
	//   - "none": no cache — path is re-parsed on every log line (use to profile cache impact)
	FilepathCacheType string `mapstructure:"filepath_cache_type,omitempty"`

	// DisableMapPools disables sync.Pool reuse for the short-lived maps allocated
	// during CRI line and log path parsing. Pools reduce GC pressure by reusing
	// already-allocated map objects rather than allocating new ones per log line.
	// Set to true to measure allocation cost without pooling.
	DisableMapPools bool `mapstructure:"disable_map_pools,omitempty"`

	// UseRegex forces CRI line parsing (containerd and crio formats) to use the
	// original regex-based approach instead of the hand-written scanner.
	// Set to true to profile the scanner improvement vs the original regex.
	UseRegex bool `mapstructure:"use_regex,omitempty"`
}

// Build will build a Container parser operator.
func (c Config) Build(set component.TelemetrySettings) (operator.Operator, error) {
	parserOperator, err := c.ParserConfig.Build(set)
	if err != nil {
		return nil, err
	}

	if c.Format != "" {
		switch c.Format {
		case dockerFormat, crioFormat, containerdFormat:
		default:
			return &Parser{}, stanzaerrors.NewError(
				"operator config has an invalid `format` field.",
				"ensure that the `format` field is set to one of `docker`, `crio`, `containerd`.",
				"format", c.OnError,
			)
		}
	}

	pathCache, err := buildCache(c)
	if err != nil {
		return nil, err
	}

	p := &Parser{
		ParserOperator:          parserOperator,
		format:                  c.Format,
		addMetadataFromFilepath: c.AddMetadataFromFilePath,
		cache:                   pathCache,
	}
	UseMapPools = !c.DisableMapPools
	UseRegexParsing = c.UseRegex
	var cLogEmitter helper.LogEmitter
	if metadata.StanzaSynchronousLogEmitterFeatureGate.IsEnabled() {
		cLogEmitter = helper.NewSynchronousLogEmitter(set, p.consumeEntries)
	} else {
		cLogEmitter = helper.NewBatchingLogEmitter(set, p.consumeEntries)
	}

	p.criLogEmitter = cLogEmitter
	recombineParser, err := createRecombine(set, c, cLogEmitter)
	if err != nil {
		return nil, fmt.Errorf("failed to create internal recombine config: %w", err)
	}

	p.recombineParser = recombineParser

	return p, nil
}

// buildCache returns a helper.Cache for filepath metadata, or nil when caching is disabled.
func buildCache(c Config) (helper.Cache, error) {
	if !c.AddMetadataFromFilePath {
		return nil, nil
	}

	switch c.FilepathCacheType {
	case CacheTypeNone:
		return nil, nil
	case CacheTypeLRU:
		lruCache, _ := lru.New[string, any](defaultPathCacheSize)
		return &lruCacheAdapter{cache: lruCache}, nil
	case CacheTypeSyncMap, "":
		return helper.NewSyncMapCache(defaultPathCacheSize, 0), nil
	default:
		return nil, fmt.Errorf("invalid filepath_cache_type %q: must be one of %q, %q, %q",
			c.FilepathCacheType, CacheTypeSyncMap, CacheTypeLRU, CacheTypeNone)
	}
}

// createRecombine creates an internal recombine operator which outputs to an async helper.LogEmitter
// the equivalent recombine config:
//
//	combine_field: body
//	combine_with: ""
//	is_last_entry: attributes.logtag == 'F'
//	max_log_size: 1048576 (1MiB)
//	source_identifier: attributes["log.file.path"]
//	type: recombine
func createRecombine(set component.TelemetrySettings, c Config, cLogEmitter helper.LogEmitter) (operator.Operator, error) {
	recombineParserCfg := createRecombineConfig(c)
	recombineParser, err := recombineParserCfg.Build(set)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve internal recombine config: %w", err)
	}

	recombineParser.SetOutputIDs([]string{cLogEmitter.ID()})
	if err := recombineParser.SetOutputs([]operator.Operator{cLogEmitter}); err != nil {
		return nil, errors.New("failed to set outputs of internal recombine")
	}

	return recombineParser, nil
}

func createRecombineConfig(c Config) *recombine.Config {
	recombineParserCfg := recombine.NewConfigWithID(recombineInternalID)
	recombineParserCfg.IsLastEntry = recombineIsLastEntry
	recombineParserCfg.CombineField = entry.NewBodyField()
	recombineParserCfg.CombineWith = ""
	recombineParserCfg.SourceIdentifier = entry.NewAttributeField(recombineSourceIdentifier)
	recombineParserCfg.MaxLogSize = c.MaxLogSize
	recombineParserCfg.MaxBatchSize = 0
	recombineParserCfg.MaxUnmatchedBatchSize = 0

	return recombineParserCfg
}

// lruCacheAdapter wraps hashicorp/golang-lru to implement helper.Cache.
type lruCacheAdapter struct {
	cache *lru.Cache[string, any]
}

func (a *lruCacheAdapter) Get(key string) any {
	val, ok := a.cache.Get(key)
	if !ok {
		return nil
	}
	return val
}

func (a *lruCacheAdapter) Add(key string, data any) bool {
	return a.cache.Add(key, data)
}

func (a *lruCacheAdapter) Copy() map[string]any {
	keys := a.cache.Keys()
	m := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := a.cache.Get(k); ok {
			m[k] = v
		}
	}
	return m
}

func (a *lruCacheAdapter) MaxSize() uint16 {
	return uint16(a.cache.Len())
}

func (a *lruCacheAdapter) Stop() {}
