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
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"script": script, "feeds": feeds, "cards": cards})
	cmd := exec.Command(node, "-e", `const fs=require('fs'),vm=require('vm');const i=JSON.parse(fs.readFileSync(0,'utf8'));const cards=(i.cards||[]).map(c=>({hidden:c.hidden,getBoundingClientRect(){return {width:c.hidden?0:100,height:100}},querySelectorAll(s){if(s!=='a[href]')throw Error('selector');return c.links.map(h=>({getAttribute(k){if(k!=='href')throw Error('private field');return h}}))}}));const c=vm.createContext({URL,window:{location:{origin:'https://www.xiaohongshu.com'},__INITIAL_STATE__:{search:{feeds:i.feeds}}},document:{querySelectorAll(s){if(s!=='section.note-item')throw Error('broad selector');return cards}},getComputedStyle:()=>({display:'block',visibility:'visible'})});process.stdout.write(JSON.stringify(vm.runInContext(i.script,c,{timeout:1000})));`)
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
