package xiaohongshu

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

func diagnosticTestStart(t *testing.T) (context.Context, *SearchDiagnostics, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", path)
	ctx, d := StartSearchDiagnostics(context.Background())
	if d == nil {
		t.Fatal("logger not enabled")
	}
	t.Cleanup(d.Close)
	return ctx, d, path
}

func diagnosticTestEvents(t *testing.T, path string) []searchDiagnosticEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []searchDiagnosticEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var event searchDiagnosticEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

// Simulated operations use the production stage/lifecycle wrappers. No browser,
// HTTP client, credentials or XHS connection is constructed by these tests.
func diagnosticFakeSearch(ctx context.Context, d *SearchDiagnostics, probe func() (bool, error)) string {
	d.Mark("search_handler_start")
	defer d.Finish(ctx, true, false)
	d.Mark("browser_context_created")
	defer d.CloseResource(ctx, "browser", true, false, func() error { return nil })
	d.Mark("page_context_created")
	defer d.CloseResource(ctx, "page", true, false, func() error { return nil })
	d.Step(ctx, "navigation", func() {})
	d.Mark("search_request_submitted")
	d.Probe(probe)
	d.Step(ctx, "stable_wait", func() {})
	d.Step(ctx, "result_state_wait", func() {})
	var data string
	d.Step(ctx, "extraction", func() { data = `[{"modelType":"note","id":"synthetic"}]` })
	d.Begin("parse_start")
	var feeds []Feed
	err := json.Unmarshal([]byte(data), &feeds)
	d.ParseEnd(err)
	d.Mark("response_ready")
	return onlyNotes(feeds)[0].ID
}

func TestSearchDiagnosticsDisabled(t *testing.T) {
	t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", "")
	parent := context.Background()
	ctx, d := StartSearchDiagnostics(parent)
	if ctx != parent || d != nil {
		t.Fatal("disabled diagnostics changed context")
	}
	got := diagnosticFakeSearch(ctx, d, func() (bool, error) { t.Fatal("disabled probe executed"); return false, nil })
	if got != "synthetic" {
		t.Fatal(got)
	}
	// Exercise actual Search's pre-navigation error path, with no browser.
	_, err := (&SearchAction{}).Search(ctx, "synthetic query", FilterOption{NoteType: "invalid"})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatal("search error changed")
	}
}

func TestSearchDiagnosticsWriteFailures(t *testing.T) {
	t.Run("open failure", func(t *testing.T) {
		t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", t.TempDir())
		ctx, d := StartSearchDiagnostics(context.Background())
		if d != nil {
			t.Fatal("directory accepted")
		}
		if diagnosticFakeSearch(ctx, d, func() (bool, error) { return true, nil }) != "synthetic" {
			t.Fatal("changed result")
		}
	})
	t.Run("write failure", func(t *testing.T) {
		ctx, d, _ := diagnosticTestStart(t)
		_ = d.file.Close() // force an actual failed Write, not just open failure
		if diagnosticFakeSearch(ctx, d, func() (bool, error) { return true, nil }) != "synthetic" {
			t.Fatal("changed result")
		}
		if d.file != nil {
			t.Fatal("failed writer not disabled")
		}
	})
}

func TestSearchDiagnosticsNormalOrder(t *testing.T) {
	ctx, d, path := diagnosticTestStart(t)
	if diagnosticFakeSearch(ctx, d, func() (bool, error) { return true, nil }) != "synthetic" {
		t.Fatal("result changed")
	}
	events := diagnosticTestEvents(t, path)
	var stages []string
	for _, e := range events {
		stages = append(stages, e.Stage)
	}
	want := []string{"search_handler_start", "browser_context_created", "page_context_created", "navigation_start", "navigation_end", "search_request_submitted", "results_visible_probe", "stable_wait_start", "stable_wait_end", "result_state_wait_start", "result_state_wait_end", "extraction_start", "extraction_end", "parse_start", "parse_end", "response_ready", "close_reason", "page_close_start", "page_close_end", "close_reason", "browser_close_start", "browser_close_end", "handler_return"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("stages: %v", stages)
	}
	for _, e := range events {
		if e.Stage == "close_reason" && e.Reason != "normal_return" {
			t.Fatal(e)
		}
	}
	// A small fixture excerpt can be printed by the explicit offline test run.
	for _, e := range events[3:11] {
		b, _ := json.Marshal(e)
		t.Log(string(b))
	}
}

func TestSearchDiagnosticsDeadline(t *testing.T) {
	ctx, d, path := diagnosticTestStart(t)
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	sentinel := &struct{ label string }{"unchanged panic"}
	func() {
		defer func() {
			if recover() != sentinel {
				t.Fatal("panic replaced or swallowed")
			}
		}()
		defer d.CloseResource(ctx, "browser", false, false, func() error { return nil })
		defer d.ObserveContext(expired)
		d.Step(expired, "stable_wait", func() { panic(sentinel) })
	}()
	events := diagnosticTestEvents(t, path)
	if events[0].Stage != "stable_wait_start" || events[1].Stage != "stable_wait_end" || events[1].ErrorClass != "context_deadline" {
		t.Fatal(events)
	}
	seen, reason := false, false
	for _, e := range events {
		seen = seen || e.Stage == "context_deadline"
		reason = reason || (e.Stage == "close_reason" && e.Reason == "context_deadline")
	}
	if !seen || !reason {
		t.Fatal(events)
	}
}

func TestSearchDiagnosticsCancellation(t *testing.T) {
	ctx, d, path := diagnosticTestStart(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	d.ObserveContext(cancelled)
	d.Finish(ctx, false, false)
	events := diagnosticTestEvents(t, path)
	if events[0].Stage != "context_cancelled" || events[1].Reason != "context_cancelled" {
		t.Fatal(events)
	}
}

func TestSearchDiagnosticsProbeIsObservational(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		probe      func() (bool, error)
	}{
		{"false", "false", func() (bool, error) { return false, nil }},
		{"error", "unknown", func() (bool, error) { return false, errors.New("secret payload") }},
		{"panic", "unknown", func() (bool, error) { panic("secret payload") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, d, path := diagnosticTestStart(t)
			if diagnosticFakeSearch(ctx, d, tc.probe) != "synthetic" {
				t.Fatal("probe changed result")
			}
			found := false
			for _, e := range diagnosticTestEvents(t, path) {
				if e.Stage == "results_visible_probe" {
					found = true
					if e.Status != tc.want {
						t.Fatal(e)
					}
				}
			}
			if !found {
				t.Fatal("missing probe")
			}
		})
	}
}

func TestSearchDiagnosticsPrivacy(t *testing.T) {
	ctx, d, path := diagnosticTestStart(t)
	secret := "cookie token xsec_token 13800138000 real_account query原文 author body"
	d.Mark(secret) // unknown stage must be dropped entirely
	d.event("handler_return", secret, secret, secret)
	d.ParseEnd(errors.New(secret))
	d.Probe(func() (bool, error) { return false, errors.New(secret) })
	func() { defer func() { _ = recover() }(); d.Step(ctx, "extraction", func() { panic(secret) }) }()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range strings.Fields(secret) {
		if strings.Contains(string(raw), word) {
			t.Fatalf("sensitive field leaked: %s", word)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		for key := range fields {
			switch key {
			case "request_id", "elapsed_ms", "stage", "status", "error_class", "reason":
			default:
				t.Fatalf("extra field %s", key)
			}
		}
	}
}

func TestSearchDiagnosticsPanicAndCleanup(t *testing.T) {
	ctx, d, path := diagnosticTestStart(t)
	sentinel := errors.New("original panic")
	var closed []string
	func() {
		defer func() {
			if recover() != sentinel {
				t.Fatal("original panic not propagated")
			}
		}()
		defer d.Finish(ctx, false, false)
		defer d.CloseResource(ctx, "browser", false, false, func() error { closed = append(closed, "browser"); return nil })
		defer d.CloseResource(ctx, "page", false, false, func() error { closed = append(closed, "page"); return nil })
		d.Step(ctx, "extraction", func() { panic(sentinel) })
	}()
	if !reflect.DeepEqual(closed, []string{"page", "browser"}) {
		t.Fatal(closed)
	}
	events := diagnosticTestEvents(t, path)
	if events[len(events)-1].Reason != "panic_unwind" {
		t.Fatal(events)
	}
}

func TestSearchDiagnosticsRequestAndMonotonicClock(t *testing.T) {
	ctx, d, path := diagnosticTestStart(t)
	if SearchDiagnosticsFrom(ctx) != d {
		t.Fatal("context logger missing")
	}
	d.Mark("search_handler_start")
	d.Mark("response_ready")
	_, other := StartSearchDiagnostics(context.Background())
	defer other.Close()
	other.Mark("search_handler_start")
	events := diagnosticTestEvents(t, path)
	if len(events[0].RequestID) != 32 || events[0].RequestID != events[1].RequestID || events[0].RequestID == events[2].RequestID {
		t.Fatal("request IDs")
	}
	if events[0].ElapsedMS < 0 || events[1].ElapsedMS < events[0].ElapsedMS {
		t.Fatal("non-monotonic")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}

func TestSearchDiagnosticsPreservesContext(t *testing.T) {
	_, _, _ = diagnosticTestStart(t)
	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	ctx, d := StartSearchDiagnostics(parent)
	defer d.Close()
	want, _ := parent.Deadline()
	got, _ := ctx.Deadline()
	if !got.Equal(want) || ctx.Done() != parent.Done() {
		t.Fatal("context boundary changed")
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("cancellation lost")
	}
}

func TestSearchDiagnosticsRejectsUnsafeDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", link)
	_, d := StartSearchDiagnostics(context.Background())
	if d != nil {
		t.Fatal("symlink accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "preserve" {
		t.Fatal("target modified")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", target)
	_, d = StartSearchDiagnostics(context.Background())
	if d != nil {
		t.Fatal("public destination accepted")
	}
}

func TestSearchDiagnosticsProductionWaitContract(t *testing.T) {
	raw, err := os.ReadFile("search.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	// Guard the explicitly frozen timeout, navigation count and readiness calls.
	for text, count := range map[string]int{
		"Timeout(60 * time.Second)":                                     2,
		"page.MustNavigate(searchURL)":                                  1,
		"page.MustWaitStable()":                                         1,
		"page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)": 1,
		"waitFeedsChanged(page, before, 15*time.Second)":                1,
	} {
		if strings.Count(s, text) != count {
			t.Fatalf("frozen call changed: %s", text)
		}
	}
	for _, stage := range []string{"navigation", "stable_wait", "result_state_wait", "extraction"} {
		if !strings.Contains(s, `diagnostics.Step(page.GetContext(), "`+stage+`",`) {
			t.Fatal("missing production stage", stage)
		}
	}
	if !strings.Contains(s, "defer diagnostics.ObserveContext(page.GetContext())") || !strings.Contains(s, "defer diagnostics.ProbeResults(page)") {
		t.Fatal("missing unwind observations")
	}
}

// This fake replaces ALL Rod transport I/O, including navigation. No browser
// process or network connection exists, even though the real Search runs.
type diagnosticFakeCDP struct {
	mu                          sync.Mutex
	events                      chan *cdp.Event
	navigationCalls, probeCalls int
	probeError                  bool
	probeVisible                bool
	extractionSource            string
	extractionJSON              *string
}

func (f *diagnosticFakeCDP) Event() <-chan *cdp.Event { return f.events }
func (f *diagnosticFakeCDP) Call(ctx context.Context, session, method string, params interface{}) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch method {
	case "Target.createTarget":
		return []byte(`{"targetId":"fake"}`), nil
	case "Target.attachToTarget":
		return []byte(`{"sessionId":"fake"}`), nil
	case "Page.navigate":
		f.navigationCalls++
		return []byte(`{"frameId":"fake"}`), nil
	case "DOMSnapshot.captureSnapshot":
		return []byte(`{"documents":[],"strings":["unchanged"]}`), nil
	case "Runtime.evaluate":
		req := params.(proto.RuntimeEvaluate)
		if req.Expression == "window" {
			return []byte(`{"result":{"type":"object","objectId":"window"}}`), nil
		}
		f.probeCalls++
		if f.probeError {
			return nil, errors.New("synthetic observer failure")
		}
		value := "false"
		if f.probeVisible {
			value = "true"
		}
		return []byte(`{"result":{"type":"boolean","value":` + value + `}}`), nil
	case "Runtime.callFunctionOn":
		req := params.(proto.RuntimeCallFunctionOn)
		if strings.Contains(req.FunctionDeclaration, "JSON.stringify(feedsData)") {
			data := `[{"modelType":"note","id":"synthetic"}]`
			if f.extractionJSON != nil {
				data = *f.extractionJSON
			}
			source := f.extractionSource
			if source == "" {
				source = "value"
			}
			var value any = data
			kind := "string"
			if len(req.Arguments) > 0 && req.Arguments[0].Value.Bool() {
				value = map[string]any{"data": data, "source": source}
				kind = "object"
			}
			return json.Marshal(map[string]any{"result": map[string]any{"type": kind, "value": value}})
		}
		if !req.ReturnByValue {
			return []byte(`{"result":{"type":"function","objectId":"helper"}}`), nil
		}
		return []byte(`{"result":{"type":"boolean","value":true}}`), nil
	}
	return []byte(`{}`), nil
}

func TestSearchDiagnosticsRealSearchWithFakeTransport(t *testing.T) {
	for _, mode := range []string{"disabled", "enabled", "open_failure", "write_failure", "probe_false", "probe_error"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
			if mode == "disabled" {
				path = ""
			}
			if mode == "open_failure" {
				path = t.TempDir()
			}
			t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", path)
			ctx, d := StartSearchDiagnostics(context.Background())
			defer d.Close()
			if mode == "write_failure" {
				_ = d.file.Close()
			}
			fake := &diagnosticFakeCDP{events: make(chan *cdp.Event), probeError: mode == "probe_error", probeVisible: mode != "probe_false"}
			defer close(fake.events)
			// Explicit Client ensures Rod never launches/downloads a browser.
			browser := rod.New().Client(fake).NoDefaultDevice().MustConnect()
			page := browser.MustPage()
			action := NewSearchAction(page)
			deadline, ok := action.page.GetContext().Deadline()
			if !ok || time.Until(deadline) > 60*time.Second || time.Until(deadline) < 59*time.Second {
				t.Fatal("search timeout changed")
			}
			got, err := action.Search(ctx, "synthetic sensitive query")
			if err != nil || len(got) != 1 || got[0].ID != "synthetic" {
				t.Fatalf("changed search result: %v %v", got, err)
			}
			fake.mu.Lock()
			calls, probes := fake.navigationCalls, fake.probeCalls
			fake.mu.Unlock()
			if calls != 1 {
				t.Fatalf("navigation/retry changed: %d", calls)
			}
			if mode == "disabled" || mode == "open_failure" {
				if probes != 0 {
					t.Fatal("disabled probe executed")
				}
				return
			}
			if probes != 2 {
				t.Fatalf("probe repeated or missing: %d", probes)
			}
			if mode == "write_failure" {
				return
			}
			events := diagnosticTestEvents(t, path)
			want := []string{"navigation_start", "navigation_end", "search_request_submitted", "results_visible_probe", "stable_wait_start", "stable_wait_end", "results_visible_probe", "result_state_wait_start", "result_state_wait_end", "extraction_start", "extraction_end", "parse_start", "parse_end", "zero_result_provenance"}
			var stages []string
			for _, event := range events {
				stages = append(stages, event.Stage)
			}
			if !reflect.DeepEqual(stages, want) {
				t.Fatal(stages)
			}
			status := "true"
			if mode == "probe_false" {
				status = "false"
			}
			if mode == "probe_error" {
				status = "unknown"
			}
			if events[3].Status != status || events[6].Status != status {
				t.Fatal("probe observation incorrect")
			}
		})
	}
}

func TestSearchDiagnosticsCloseErrorAndPanic(t *testing.T) {
	ctx, d, path := diagnosticTestStart(t)
	d.CloseResource(ctx, "page", true, false, func() error { return errors.New("private close error") })
	events := diagnosticTestEvents(t, path)
	if events[len(events)-1].Stage != "page_close_end" || events[len(events)-1].Status != "failure" {
		t.Fatal(events)
	}
	sentinel := errors.New("private close panic")
	func() {
		defer func() {
			if recover() != sentinel {
				t.Fatal("close panic changed")
			}
		}()
		defer d.CloseResource(ctx, "browser", true, false, func() error { return nil })
		d.CloseResource(ctx, "page", true, false, func() error { panic(sentinel) })
	}()
	events = diagnosticTestEvents(t, path)
	if events[len(events)-3].Reason != "panic_unwind" {
		t.Fatal("outer cleanup lost panic reason", events)
	}
}
