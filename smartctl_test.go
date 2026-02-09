// Copyright 2024 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tidwall/gjson"
)

var updateGolden = flag.Bool("update", false, "update .golden files")

func TestBuildDeviceLabel(t *testing.T) {
	tests := []struct {
		deviceName    string
		deviceType    string
		expectedLabel string
	}{
		{"/dev/bus/0", "megaraid,1", "bus_0_megaraid_1"},
		{"/dev/sda", "auto", "sda"},
		{"/dev/disk/by-id/ata-CT500MX500SSD1_ABCDEFGHIJ", "auto", "ata-CT500MX500SSD1_ABCDEFGHIJ"},
		// Some cases extracted from smartctl docs. Are these the prettiest?
		// Probably not. Are they unique enough. Definitely.
		{"/dev/sg1", "cciss,1", "sg1_cciss_1"},
		{"/dev/bsg/sssraid0", "sssraid,0,1", "bsg_sssraid0_sssraid_0_1"},
		{"/dev/cciss/c0d0", "cciss,0", "cciss_c0d0_cciss_0"},
		{"/dev/sdb", "aacraid,1,0,4", "sdb_aacraid_1_0_4"},
		{"/dev/twl0", "3ware,1", "twl0_3ware_1"},
	}

	for _, test := range tests {
		result := buildDeviceLabel(test.deviceName, test.deviceType)
		if result != test.expectedLabel {
			t.Errorf("deviceName=%v deviceType=%v expected=%v result=%v", test.deviceName, test.deviceType, test.expectedLabel, result)
		}
	}
}

// collectMetrics runs the full Collect() pipeline on JSON data and returns
// sorted, deterministic metric output.
func collectMetrics(t *testing.T, jsonData []byte) string {
	t.Helper()
	json := gjson.Parse(string(jsonData))
	ch := make(chan prometheus.Metric, 10000)
	smart := NewSMARTctl(slog.Default(), json, ch)
	smart.Collect()
	close(ch)

	var lines []string
	for m := range ch {
		metric := &dto.Metric{}
		if err := m.Write(metric); err != nil {
			t.Fatalf("failed to write metric: %v", err)
		}
		lines = append(lines, formatMetric(m.Desc(), metric))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// formatMetric serializes a single metric to a deterministic text format.
func formatMetric(desc *prometheus.Desc, m *dto.Metric) string {
	fqName := extractFqName(desc.String())

	labels := m.GetLabel()
	labelPairs := make([]string, 0, len(labels))
	for _, lp := range labels {
		labelPairs = append(labelPairs, fmt.Sprintf("%s=%q", lp.GetName(), lp.GetValue()))
	}
	sort.Strings(labelPairs)

	var v float64
	switch {
	case m.Gauge != nil:
		v = m.GetGauge().GetValue()
	case m.Counter != nil:
		v = m.GetCounter().GetValue()
	case m.Untyped != nil:
		v = m.GetUntyped().GetValue()
	}

	labelStr := strings.Join(labelPairs, ",")
	if labelStr != "" {
		return fmt.Sprintf("%s{%s} %s", fqName, labelStr, strconv.FormatFloat(v, 'g', -1, 64))
	}
	return fmt.Sprintf("%s %s", fqName, strconv.FormatFloat(v, 'g', -1, 64))
}

// extractFqName parses the fqName from a prometheus.Desc.String() output.
func extractFqName(descStr string) string {
	const prefix = `fqName: "`
	i := strings.Index(descStr, prefix)
	if i < 0 {
		return "unknown"
	}
	s := descStr[i+len(prefix):]
	j := strings.Index(s, `"`)
	if j < 0 {
		return "unknown"
	}
	return s[:j]
}

// diff produces a simple line-by-line diff between want and got.
func diff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")

	var b strings.Builder
	maxLen := len(wantLines)
	if len(gotLines) > maxLen {
		maxLen = len(gotLines)
	}
	for i := 0; i < maxLen; i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			if w != "" {
				fmt.Fprintf(&b, "- %s\n", w)
			}
			if g != "" {
				fmt.Fprintf(&b, "+ %s\n", g)
			}
		}
	}
	return b.String()
}

func TestGoldenFiles(t *testing.T) {
	jsonFiles, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(jsonFiles) == 0 {
		t.Fatal("no testdata/*.json files found")
	}

	for _, jsonFile := range jsonFiles {
		basename := filepath.Base(jsonFile)
		goldenFile := filepath.Join("testdata", "golden", strings.TrimSuffix(basename, ".json")+".golden")

		t.Run(basename, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(jsonFile)
			if err != nil {
				t.Fatalf("reading %s: %v", jsonFile, err)
			}

			got := collectMetrics(t, data)

			if *updateGolden {
				if err := os.MkdirAll(filepath.Join("testdata", "golden"), 0755); err != nil {
					t.Fatalf("creating golden dir: %v", err)
				}
				if err := os.WriteFile(goldenFile, []byte(got), 0644); err != nil {
					t.Fatalf("writing golden file: %v", err)
				}
				return
			}

			want, err := os.ReadFile(goldenFile)
			if err != nil {
				t.Fatalf("reading golden file %s: %v (run with -update to create)", goldenFile, err)
			}

			if string(want) != got {
				t.Errorf("output mismatch for %s:\n%s", basename, diff(string(want), got))
			}
		})
	}
}
