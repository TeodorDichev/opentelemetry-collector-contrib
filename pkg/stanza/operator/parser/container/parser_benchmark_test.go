// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"regexp"
	"testing"

	"go.opentelemetry.io/collector/component/componenttest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/entry"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/fileconsumer/attrs"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/helper"
)

const (
	benchLogPath       = "/var/log/pods/default_mypod_49cc7c1fd3702c40b2686ea7486091d3/mycontainer/0.log"
	benchContainerdLog = "2024-01-15T10:30:00.000Z stdout F this is a test log line with realistic content"
	benchCRIOLog       = "2024-01-15T10:30:00.000000000+00:00 stdout F this is a test log line with realistic content"
)

// old regex patterns kept here only for benchmarking comparison
var (
	benchCRIOMatcher       = regexp.MustCompile(`^(?P<time>[^ Z]+) (?P<stream>stdout|stderr) (?P<logtag>[^ ]*) ?(?P<log>.*)$`)
	benchContainerdMatcher = regexp.MustCompile(`^(?P<time>[^ ^Z]+Z) (?P<stream>stdout|stderr) (?P<logtag>[^ ]*) ?(?P<log>.*)$`)
	benchPathMatcher       = regexp.MustCompile(`^.*(\/|\\)(?P<namespace>[^_]+)_(?P<pod_name>[^_]+)_(?P<uid>[a-f0-9\-]+)(\/|\\)(?P<container_name>[^\._]+)(\/|\\)(?P<restart_count>\d+)\.log(\.\d{8}-\d{6})?$`)
)

// BenchmarkContainerdCRIParsing compares the hand-written CRI scanner against the original
// regex-based approach for containerd log line formats.
func BenchmarkContainerdCRIParsing(b *testing.B) {
	benchmarks := []struct {
		name  string
		input string
		re    *regexp.Regexp
	}{
		{"Containerd", benchContainerdLog, benchContainerdMatcher},
	}

	for _, bm := range benchmarks {
		b.Run("Regex/"+bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = helper.MatchValues(bm.input, bm.re)
			}
		})

		b.Run("NoRegex/"+bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = parseContainerd(bm.input)
			}
		})
	}
}

// BenchmarkCRIOParsing compares the hand-written CRIO scanner against the original
// regex-based approach for crio log line formats.
func BenchmarkCRIOParsing(b *testing.B) {
	benchmarks := []struct {
		name  string
		input string
		re    *regexp.Regexp
	}{
		{"CRIO", benchCRIOLog, benchCRIOMatcher},
	}

	for _, bm := range benchmarks {
		b.Run("Regex/"+bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = helper.MatchValues(bm.input, bm.re)
			}
		})

		b.Run("NoRegex/"+bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = parseCRIO(bm.input)
			}
		})
	}
}

// BenchmarkLogPathParsing compares the hand-written log path parser against the
// original regex-based approach.
func BenchmarkLogPathParsing(b *testing.B) {
	benchmarks := []struct {
		name  string
		input string
	}{
		{"Standard", benchLogPath},
		{"Rotated", benchLogPath + ".20240115-103000"},
	}

	for _, bm := range benchmarks {
		b.Run("Regex/"+bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = helper.MatchValues(bm.input, benchPathMatcher)
			}
		})

		b.Run("NoRegex/"+bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = parseLogPath(bm.input)
			}
		})
	}
}

// BenchmarkMapPools compares CRI parsing with and without sync.Pool map reuse.
func BenchmarkMapPools(b *testing.B) {
	for _, disabled := range []bool{false, true} {
		name := "WithPools"
		if disabled {
			name = "WithoutPools"
		}
		b.Run(name, func(b *testing.B) {
			cfg := NewConfigWithID("bench")
			cfg.Format = containerdFormat
			cfg.AddMetadataFromFilePath = false
			cfg.DisableMapPools = disabled
			set := componenttest.NewNopTelemetrySettings()
			op, err := cfg.Build(set)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = op.Stop() })
			p := op.(*Parser)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = parseContainerd(benchContainerdLog)
				_ = p // keep p alive so Build side-effects stay
			}
		})
	}
}

// buildBenchParser creates a parser with the given cache type and stops it after the benchmark.
func buildBenchParser(b *testing.B, cacheType string) *Parser {
	b.Helper()
	cfg := NewConfigWithID("bench")
	cfg.Format = containerdFormat
	cfg.AddMetadataFromFilePath = true
	cfg.FilepathCacheType = cacheType
	set := componenttest.NewNopTelemetrySettings()
	op, err := cfg.Build(set)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = op.Stop() })
	return op.(*Parser)
}

// BenchmarkCacheTypes compares the full containerd hot path across all cache configurations.
func BenchmarkCacheTypes(b *testing.B) {
	for _, ct := range []string{CacheTypeSyncMap, CacheTypeLRU, CacheTypeNone} {
		b.Run(ct, func(b *testing.B) {
			p := buildBenchParser(b, ct)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					e := entry.New()
					e.Body = benchContainerdLog
					e.Attributes = map[string]any{attrs.LogFilePath: benchLogPath}
					_ = p.Process(context.Background(), e)
				}
			})
		})
	}
}
