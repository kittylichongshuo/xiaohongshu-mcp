package xiaohongshu

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
)

// Executes production JS in a synthetic Node VM, never a browser/network.
func runSearchFixture(t *testing.T, script string, feeds any, cards []any) json.RawMessage {
	t.Helper()
	return runProductionSearchFixture(t, script, feeds, cards, nil)
}

func runProductionSearchFixture(t *testing.T, script string, feeds any, cards []any, overrides map[string]any) json.RawMessage {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	inputFields := map[string]any{"script": script, "feeds": feeds, "cards": cards}
	for key, value := range overrides {
		inputFields[key] = value
	}
	input, _ := json.Marshal(inputFields)
	cmd := exec.Command(node, "testdata/search_result_dom.js")
	cmd.Stdin = strings.NewReader(string(input))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("offline fixture failed", err)
	}
	return out
}

func TestRenderedSearchURLRules(t *testing.T) {
	for _, tc := range []struct {
		name, href string
		valid      bool
	}{
		{"relative", "/explore/note1?xsec_token=public%2Baccess", true},
		{"absolute", "https://www.xiaohongshu.com/search_result/note2?xsec_token=access", true},
		{"discovery", "https://xiaohongshu.com/discovery/item/note3?xsec_token=access", true},
		{"missing ID", "/explore/?xsec_token=access", false},
		{"missing token", "/explore/note1", false},
		{"blank token", "/explore/note1?xsec_token=%20", false},
		{"wrong route", "/user/profile/person?xsec_token=access", false},
		{"external", "https://other.invalid/explore/note1?xsec_token=access", false},
		{"suffix host", "https://www.xiaohongshu.com.other.invalid/explore/note1?xsec_token=access", false},
		{"credentials", "https://private@www.xiaohongshu.com/explore/note1?xsec_token=access", false},
		{"javascript", "javascript:void(0)", false},
		{"http", "http://www.xiaohongshu.com/explore/note1?xsec_token=access", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cards := []any{map[string]any{"links": []string{tc.href}}}
			out := runSearchFixture(t, "("+renderedSearchFeedsJS+")()", nil, cards)
			var feeds []Feed
			if json.Unmarshal(out, &feeds) != nil {
				t.Fatal("invalid feeds")
			}
			if (len(feeds) == 1) != tc.valid {
				t.Fatal("unexpected acceptance")
			}
			ready := runSearchFixture(t, searchReadinessJS, nil, cards)
			expected := `"none"`
			if tc.valid {
				expected = `"DOM"`
			}
			if string(ready) != expected {
				t.Fatal("readiness/extraction mismatch")
			}
			if tc.name == "relative" && feeds[0].XsecToken != "public+access" {
				t.Fatal("query parsing changed token")
			}
		})
	}
}

func TestRenderedSearchDedupeOrderPrivacy(t *testing.T) {
	cards := []any{
		map[string]any{"links": []string{"/user/profile/private-author", "/explore/first?xsec_token=runtime-access"}},
		map[string]any{"links": []string{"/explore/first?xsec_token=other"}},
		map[string]any{"hidden": true, "links": []string{"/explore/hidden?xsec_token=access"}},
		map[string]any{"links": []string{"/search_result/second?xsec_token=access"}, "body": "private-body", "avatar": "private-avatar"},
	}
	out := runSearchFixture(t, "("+renderedSearchFeedsJS+")()", nil, cards)
	var feeds []Feed
	_ = json.Unmarshal(out, &feeds)
	if len(feeds) != 2 || feeds[0].ID != "first" || feeds[1].ID != "second" || feeds[0].Index != 0 || feeds[1].Index != 3 {
		t.Fatal("order/dedupe")
	}
	for _, f := range feeds {
		if f.ModelType != "note" || !reflect.DeepEqual(f.NoteCard, NoteCard{}) {
			t.Fatal("unnecessary fields")
		}
	}
	for _, secret := range []string{"private-", "href", "https://", "<html"} {
		if strings.Contains(string(out), secret) {
			t.Fatal("private fields leaked")
		}
	}
}

func TestSearchReadinessStateOrDOM(t *testing.T) {
	note := map[string]any{"modelType": "note", "id": "state-note", "xsecToken": "runtime-access"}
	for _, tc := range []struct {
		name  string
		feeds any
		cards []any
		want  string
	}{
		{"value", map[string]any{"value": []any{note}}, nil, "state"},
		{"backing", map[string]any{"value": []any{}, "_value": []any{note}}, nil, "state"},
		{"DOM", map[string]any{"value": []any{}, "_value": []any{}}, []any{map[string]any{"links": []string{"/explore/dom?xsec_token=access"}}}, "DOM"},
		{"invalid state", map[string]any{"value": []any{map[string]any{"modelType": "note", "id": "missing-token"}}}, nil, "none"},
		{"empty", nil, nil, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runSearchFixture(t, searchReadinessJS, tc.feeds, tc.cards)
			if string(out) != `"`+tc.want+`"` {
				t.Fatal(string(out))
			}
		})
	}
}

func TestBoundedSearchReadiness(t *testing.T) {
	if searchReadinessTimeout != 20*time.Second {
		t.Fatal("readiness budget changed")
	}
	t.Run("immediate ready", func(t *testing.T) {
		calls := 0
		err := waitForSearchReady(context.Background(), time.Second, func(context.Context) (bool, error) { calls++; return true, nil })
		if err != nil || calls != 1 {
			t.Fatal(err, calls)
		}
	})
	t.Run("candidate appears", func(t *testing.T) {
		calls := 0
		start := time.Now()
		err := waitForSearchReady(context.Background(), time.Second, func(context.Context) (bool, error) { calls++; return calls == 2, nil })
		if err != nil || calls != 2 || time.Since(start) > time.Second {
			t.Fatal(err, calls)
		}
	})
	t.Run("empty budget", func(t *testing.T) {
		start := time.Now()
		err := waitForSearchReady(context.Background(), 10*time.Millisecond, func(context.Context) (bool, error) { return false, nil })
		if err != nil || time.Since(start) > time.Second {
			t.Fatal(err)
		}
	})
	t.Run("blocked probe bounded", func(t *testing.T) {
		err := waitForSearchReady(context.Background(), 10*time.Millisecond, func(ctx context.Context) (bool, error) { <-ctx.Done(); return false, ctx.Err() })
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := waitForSearchReady(ctx, time.Second, func(context.Context) (bool, error) { t.Fatal("probe after cancellation"); return false, nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("page deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		err := waitForSearchReady(ctx, time.Second, func(ctx context.Context) (bool, error) { <-ctx.Done(); return false, ctx.Err() })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
	t.Run("evaluation error", func(t *testing.T) {
		want := errors.New("evaluation failed")
		err := waitForSearchReady(context.Background(), time.Second, func(context.Context) (bool, error) { return false, want })
		if err != want {
			t.Fatal(err)
		}
	})
}

func TestSearchFixRealSearchFakeTransport(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		wantID      string
		domCalls    int
	}{
		{"state wins", `[{"id":"state","xsecToken":"access","modelType":"note","noteCard":{}}]`, "state", 0},
		{"empty state DOM", `[]`, "dom", 1},
		{"missing state DOM", ``, "dom", 1},
		{"non note state DOM", `[{"modelType":"live_v2"}]`, "dom", 1},
		{"invalid note DOM", `[{"id":"invalid","modelType":"note"}]`, "dom", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, path := diagnosticTestStart(t)
			fake := &diagnosticFakeCDP{events: make(chan *cdp.Event), extractionJSON: &tc.state, domJSON: `[{"id":"dom","xsecToken":"access","modelType":"note","noteCard":{},"index":0}]`}
			defer close(fake.events)
			page := rod.New().Client(fake).NoDefaultDevice().MustConnect().MustPage()
			got, err := NewSearchAction(page).Search(ctx, "private-query")
			if err != nil || len(got) != 1 || got[0].ID != tc.wantID || fake.domCalls != tc.domCalls || fake.stableCalls != 0 || fake.navigationCalls != 1 {
				t.Fatal("source/flow mismatch", err)
			}
			raw, _ := os.ReadFile(path)
			for _, secret := range []string{"private-query", `"id":`, `"xsecToken":`, "access"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("runtime data logged")
				}
			}
		})
	}
}

func TestSearchFixEmptyBoundedRealSearch(t *testing.T) {
	t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", "")
	empty := "[]"
	fake := &diagnosticFakeCDP{events: make(chan *cdp.Event), extractionJSON: &empty, readinessSource: "none"}
	defer close(fake.events)
	page := rod.New().Client(fake).NoDefaultDevice().MustConnect().MustPage()
	start := time.Now()
	got, err := NewSearchAction(page).Search(context.Background(), "offline-empty")
	elapsed := time.Since(start)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatal("empty results contract", err)
	}
	if elapsed < searchReadinessTimeout || elapsed > 30*time.Second {
		t.Fatal("readiness not bounded", elapsed)
	}
	if fake.navigationCalls != 1 || fake.stableCalls != 0 || fake.domCalls != 1 {
		t.Fatal("unexpected retry or stability wait")
	}
}

func TestProductionSearchAreaAndLinks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cards     []any
		overrides map[string]any
		count     int
	}{
		{"observed cover and title", []any{map[string]any{"links": []any{
			map[string]any{"href": "/explore/synthetic-note", "classes": []string{}, "hidden": true},
			map[string]any{"href": "/search_result/synthetic-note?xsec_token=synthetic%2Baccess&xsec_source=pc_search", "classes": []string{"cover", "mask", "ld"}},
			map[string]any{"href": "/search_result/synthetic-note?xsec_token=synthetic%2Baccess&xsec_source=pc_search", "classes": []string{"title"}},
		}}}, nil, 1},
		{"title href only", []any{map[string]any{"links": []any{map[string]any{"href": "https://www.xiaohongshu.com/search_result/synthetic-note?xsec_token=synthetic%2Baccess", "classes": []string{"title"}}}}}, nil, 1},
		{"wrong area", []any{map[string]any{"links": []string{"/search_result/synthetic-note?xsec_token=synthetic-access"}}}, map[string]any{"areaClasses": []string{"unrelated-area"}}, 0},
		{"wrong container", []any{map[string]any{"links": []string{"/search_result/synthetic-note?xsec_token=synthetic-access"}}}, map[string]any{"containerClasses": []string{"unrelated-list"}}, 0},
		{"wrong card", []any{map[string]any{"tag": "div", "links": []string{"/search_result/synthetic-note?xsec_token=synthetic-access"}}}, nil, 0},
		{"unrelated anchor", []any{map[string]any{"links": []any{map[string]any{"href": "/search_result/synthetic-note?xsec_token=synthetic-access", "classes": []string{"unrelated-link"}}}}}, nil, 0},
		{"hidden cover", []any{map[string]any{"links": []any{map[string]any{"href": "/search_result/synthetic-note?xsec_token=synthetic-access", "hidden": true}}}}, nil, 0},
		{"outside only", nil, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := runProductionSearchFixture(t, "("+renderedSearchFeedsJS+")()", nil, tc.cards, tc.overrides)
			var feeds []Feed
			if err := json.Unmarshal(raw, &feeds); err != nil {
				t.Fatal(err)
			}
			if len(feeds) != tc.count {
				t.Fatal("production scope mismatch", len(feeds))
			}
			if tc.count > 0 {
				if feeds[0].ID != "synthetic-note" || feeds[0].XsecToken != "synthetic+access" || feeds[0].Index != 0 || feeds[0].ModelType != "note" || !reflect.DeepEqual(feeds[0].NoteCard, NoteCard{}) {
					t.Fatal("minimal contract mismatch")
				}
			}
			readiness := runProductionSearchFixture(t, searchReadinessJS, nil, tc.cards, tc.overrides)
			expected := `"none"`
			if tc.count > 0 {
				expected = `"DOM"`
			}
			if string(readiness) != expected {
				t.Fatal("shared recognizer mismatch")
			}
		})
	}
}
