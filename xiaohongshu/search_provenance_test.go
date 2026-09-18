package xiaohongshu

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
)

func TestSearchProvenanceCountsAndPrivacy(t *testing.T) {
	_, d, path := diagnosticTestStart(t)
	secret := "sensitive-query-id-title-author-cookie-token-phone-account"
	var feeds []Feed
	if err := json.Unmarshal([]byte(`[{"modelType":"note","id":"private-id"},{"modelType":"live_v2"},{"modelType":"note"},{"modelType":"hot_query"},{},{"modelType":""},{"modelType":" "}]`), &feeds); err != nil {
		t.Fatal(err)
	}
	feeds = append(feeds, Feed{ModelType: secret}, Feed{ModelType: strings.Repeat("x", 1000)})
	before := append([]Feed(nil), feeds...)
	expected := onlyNotes(feeds)
	d.Provenance("_value", feeds, expected, true)
	events := diagnosticTestEvents(t, path)
	event := events[0]
	if event.Stage != "zero_result_provenance" || *event.RawExtractedFeedCount != 9 || *event.PostOnlyNotesCount != 2 {
		t.Fatal(event)
	}
	if !reflect.DeepEqual(event.ModelTypeCounts, map[string]int{"note": 2, "live_v2": 1, "hot_query": 1, "unknown": 5}) {
		t.Fatal(event.ModelTypeCounts)
	}
	if !reflect.DeepEqual(feeds, before) || !reflect.DeepEqual(onlyNotes(feeds), expected) {
		t.Fatal("filter or input changed")
	}
	raw, _ := os.ReadFile(path)
	for _, private := range []string{secret, "private-id", strings.Repeat("x", 1000)} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private data leaked")
		}
	}
	t.Log(string(raw))
}

func TestSearchProvenanceZeroVsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		source string
		parsed bool
	}{{"value", true}, {"_value", true}, {"none", false}, {"private-source", false}} {
		t.Run(tc.source, func(t *testing.T) {
			_, d, path := diagnosticTestStart(t)
			d.Provenance(tc.source, nil, nil, tc.parsed)
			e := diagnosticTestEvents(t, path)[0]
			want := tc.source
			if want == "private-source" {
				want = "unknown"
			}
			if e.ExtractionSource != want {
				t.Fatal(e)
			}
			if tc.parsed {
				if e.RawExtractedFeedCount == nil || *e.RawExtractedFeedCount != 0 || e.PostOnlyNotesCount == nil || *e.PostOnlyNotesCount != 0 || e.ModelTypeCounts == nil || len(e.ModelTypeCounts) != 0 {
					t.Fatal(e)
				}
			} else if e.RawExtractedFeedCount != nil || e.PostOnlyNotesCount != nil {
				t.Fatal("unavailable counts reported as zero")
			}
		})
	}
}

func TestSearchProvenanceWriteFailureIsNoOp(t *testing.T) {
	_, d, _ := diagnosticTestStart(t)
	_ = d.file.Close()
	feeds := []Feed{{ModelType: "note"}, {ModelType: "live_v2"}}
	want := onlyNotes(feeds)
	d.Provenance("value", feeds, want, true)
	if !reflect.DeepEqual(onlyNotes(feeds), want) {
		t.Fatal("write failure changed result")
	}
	var disabled *SearchDiagnostics
	disabled.Provenance("value", feeds, want, true)
}

func TestSearchProvenanceRealSearchFakeCDP(t *testing.T) {
	for _, tc := range []struct {
		name, source, data string
		raw, post          int
		wantError          bool
	}{
		{"value", "value", `[{"modelType":"note","id":"synthetic","xsecToken":"synthetic-access"},{"modelType":"live_v2"}]`, 2, 1, false},
		{"fallback", "_value", `[{"modelType":"live_v2"},{"modelType":""}]`, 2, 0, false},
		{"empty", "value", `[]`, 0, 0, false},
		{"no feeds", "none", "", 0, 0, false},
		{"invalid JSON", "_value", "{", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, path := diagnosticTestStart(t)
			fake := &diagnosticFakeCDP{events: make(chan *cdp.Event), extractionSource: tc.source, extractionJSON: &tc.data}
			defer close(fake.events)
			browser := rod.New().Client(fake).NoDefaultDevice().MustConnect()
			page := browser.MustPage()
			notes, err := NewSearchAction(page).Search(ctx, "synthetic-private-keyword")
			if (err != nil) != tc.wantError {
				t.Fatal("error semantics changed", err)
			}
			if !tc.wantError {
				var original []Feed
				if err := json.Unmarshal([]byte(func() string {
					if tc.data == "" {
						return "[]"
					}
					return tc.data
				}()), &original); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(notes, onlyNotes(original)) {
					t.Fatal("onlyNotes result changed")
				}
			}
			found := false
			for _, e := range diagnosticTestEvents(t, path) {
				if e.Stage == "zero_result_provenance" {
					found = true
					if e.ExtractionSource != tc.source {
						t.Fatal(e)
					}
					if !tc.wantError && tc.data != "" && (*e.RawExtractedFeedCount != tc.raw || *e.PostOnlyNotesCount != tc.post) {
						t.Fatal(e)
					}
					if (tc.wantError || tc.data == "") && (e.RawExtractedFeedCount != nil || e.PostOnlyNotesCount != nil) {
						t.Fatal("fabricated counts")
					}
				}
			}
			if !found {
				t.Fatal("missing provenance")
			}
			if fake.navigationCalls != 1 {
				t.Fatal("retry added")
			}
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), "synthetic-private-keyword") || strings.Contains(string(raw), "synthetic\"") {
				t.Fatal("private data leaked")
			}
		})
	}
}

func TestSearchProvenanceExtractionSelectionContract(t *testing.T) {
	raw, err := os.ReadFile("search.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, expr := range []string{"const hasValue = feeds.value !== undefined;", "const feedsData = hasValue ? feeds.value : feeds._value;", "if (feedsData)", "JSON.stringify(feedsData)", "notes := onlyNotes(feeds)"} {
		if !strings.Contains(source, expr) {
			t.Fatal("extraction/filter semantics changed", expr)
		}
	}
	// Diagnostics only carries source and the already-extracted string in memory;
	// no extra page evaluation, probe, selector or network request is introduced.
	if strings.Count(source, "page.MustEval(") != 1 {
		t.Fatal("extra extraction evaluation")
	}
}
