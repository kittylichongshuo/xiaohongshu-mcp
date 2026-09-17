package xiaohongshu

// Search diagnostics are opt-in and deliberately have no free-form payload API.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

type searchDiagnosticKey struct{}

type SearchDiagnostics struct {
	mu            sync.Mutex
	file          *os.File
	requestID     string
	start         time.Time
	contextReason string
}

// Pointer fields distinguish unavailable observations from false/zero.
type FeedContainerState struct {
	InitialStateSearchPresent *bool  `json:"initial_state_search_present,omitempty"`
	FeedsContainerPresent     *bool  `json:"feeds_container_present,omitempty"`
	ValuePresent              *bool  `json:"value_present,omitempty"`
	ValueIsArray              *bool  `json:"value_is_array,omitempty"`
	ValueCount                *int   `json:"value_count,omitempty"`
	BackingValuePresent       *bool  `json:"_value_present,omitempty"`
	BackingValueIsArray       *bool  `json:"_value_is_array,omitempty"`
	BackingValueCount         *int   `json:"_value_count,omitempty"`
	SelectedSource            string `json:"selected_source"`
	SelectedCount             *int   `json:"selected_count,omitempty"`
}

// Decode only the fixed scalar schema. Never log the incoming object.
// Invalid metadata is unavailable, not a search error or fabricated zero.
func decodeFeedContainerState(value gson.JSON) (state *FeedContainerState) {
	state = &FeedContainerState{SelectedSource: "unknown"}
	defer func() {
		if recover() != nil {
			state = &FeedContainerState{SelectedSource: "unknown"}
		}
	}()
	encoded, err := json.Marshal(value)
	if err != nil {
		return state
	}
	var parsed FeedContainerState
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		return state
	}
	switch parsed.SelectedSource {
	case "value", "_value", "none", "unknown":
	default:
		return state
	}
	if parsed.ValueIsArray == nil || !*parsed.ValueIsArray || (parsed.ValueCount != nil && *parsed.ValueCount < 0) {
		parsed.ValueCount = nil
	}
	if parsed.BackingValueIsArray == nil || !*parsed.BackingValueIsArray || (parsed.BackingValueCount != nil && *parsed.BackingValueCount < 0) {
		parsed.BackingValueCount = nil
	}
	if parsed.SelectedSource == "none" || parsed.SelectedSource == "unknown" || (parsed.SelectedCount != nil && *parsed.SelectedCount < 0) {
		parsed.SelectedCount = nil
	}
	return &parsed
}

type SearchProvenance struct {
	*FeedContainerState
	RawExtractedFeedCount *int           `json:"raw_extracted_feed_count,omitempty"`
	PostOnlyNotesCount    *int           `json:"post_onlyNotes_count,omitempty"`
	ModelTypeCounts       map[string]int `json:"model_type_counts"`
	ExtractionSource      string         `json:"extraction_source"`
}

type searchDiagnosticEvent struct {
	*SearchProvenance
	RequestID  string  `json:"request_id"`
	ElapsedMS  float64 `json:"elapsed_ms"`
	Stage      string  `json:"stage"`
	Status     string  `json:"status"`
	ErrorClass string  `json:"error_class"`
	Reason     string  `json:"reason,omitempty"`
}

// StartSearchDiagnostics preserves the parent's cancellation/deadline. An unset
// path or any open failure returns the original context and a nil/no-op logger.
// Parent directories must already exist. No fallback log destination is used.
func StartSearchDiagnostics(ctx context.Context) (context.Context, *SearchDiagnostics) {
	path := os.Getenv("XHS_SEARCH_DIAGNOSTICS_PATH")
	if path == "" {
		return ctx, nil
	}
	// Reject non-regular destinations before opening (in particular FIFOs).
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return ctx, nil
		}
	} else if !os.IsNotExist(err) {
		return ctx, nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return ctx, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return ctx, nil
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return ctx, nil
	}
	d := &SearchDiagnostics{file: f, requestID: hex.EncodeToString(id[:]), start: time.Now()}
	return context.WithValue(ctx, searchDiagnosticKey{}, d), d
}

func SearchDiagnosticsFrom(ctx context.Context) *SearchDiagnostics {
	d, _ := ctx.Value(searchDiagnosticKey{}).(*SearchDiagnostics)
	return d
}

func (d *SearchDiagnostics) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
}

func diagnosticStage(stage string) bool {
	switch stage {
	case "search_handler_start", "browser_context_created", "page_context_created",
		"navigation_start", "navigation_end", "search_request_submitted",
		"stable_wait_start", "stable_wait_end", "result_state_wait_start", "result_state_wait_end",
		"results_visible_probe", "extraction_start", "extraction_end", "parse_start", "parse_end",
		"response_ready", "handler_return", "context_deadline", "context_cancelled",
		"page_close_start", "page_close_end", "browser_close_start", "browser_close_end", "close_reason", "zero_result_provenance":
		return true
	}
	return false
}

func (d *SearchDiagnostics) event(stage, status, errorClass, reason string, provenance ...*SearchProvenance) {
	if d == nil || !diagnosticStage(stage) {
		return
	}
	switch status {
	case "success", "failure", "started", "true", "false", "unknown":
	default:
		status = "unknown"
	}
	switch errorClass {
	case "none", "operation_error", "panic_unwind", "context_deadline", "context_cancelled", "probe_error":
	default:
		errorClass = "operation_error"
	}
	switch reason {
	case "", "normal_return", "error_return", "context_deadline", "context_cancelled", "panic_unwind", "unknown":
	default:
		reason = "unknown"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return
	}
	event := searchDiagnosticEvent{RequestID: d.requestID, ElapsedMS: float64(time.Since(d.start).Nanoseconds()) / 1e6, Stage: stage, Status: status, ErrorClass: errorClass, Reason: reason}
	if stage == "zero_result_provenance" && len(provenance) == 1 {
		event.SearchProvenance = provenance[0]
	}
	data, err := json.Marshal(event)
	if err == nil {
		_, err = d.file.Write(append(data, '\n'))
	}
	if err != nil {
		_ = d.file.Close()
		d.file = nil
	}
}

func (d *SearchDiagnostics) Begin(stage string) { d.event(stage, "started", "none", "") }

func (d *SearchDiagnostics) Mark(stage string) { d.event(stage, "success", "none", "") }

// ObserveContext is called on unwind as well as normal return. It does not
// create a watcher, cancel a context, or introduce a new search deadline.
func (d *SearchDiagnostics) ObserveContext(ctx context.Context) {
	if d == nil {
		return
	}
	if ctx.Err() != nil {
		d.mu.Lock()
		if ctx.Err() == context.DeadlineExceeded {
			d.contextReason = "context_deadline"
		}
		if ctx.Err() == context.Canceled {
			d.contextReason = "context_cancelled"
		}
		d.mu.Unlock()
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		d.event("context_deadline", "failure", "context_deadline", "")
	case context.Canceled:
		d.event("context_cancelled", "failure", "context_cancelled", "")
	}
}

// Step never recovers. A completion flag records unwinding while the original
// panic continues, unchanged, to the existing MCP recovery boundary.
func (d *SearchDiagnostics) Step(ctx context.Context, stage string, fn func()) {
	if d == nil {
		fn()
		return
	}
	d.step(ctx, stage, func() error { fn(); return nil })
}

func (d *SearchDiagnostics) step(ctx context.Context, stage string, fn func() error) {
	d.event(stage+"_start", "started", "none", "")
	completed := false
	var err error
	defer func() {
		if completed {
			if err != nil {
				d.event(stage+"_end", "failure", "operation_error", "")
			} else {
				d.Mark(stage + "_end")
			}
			return
		}
		class := "panic_unwind"
		if ctx.Err() == context.DeadlineExceeded {
			class = "context_deadline"
		}
		if ctx.Err() == context.Canceled {
			class = "context_cancelled"
		}
		d.mu.Lock()
		d.contextReason = class
		d.mu.Unlock()
		d.event(stage+"_end", "failure", class, "")
		d.ObserveContext(ctx)
	}()
	err = fn()
	completed = true
}

func (d *SearchDiagnostics) ParseEnd(err error) {
	if err != nil {
		d.event("parse_end", "failure", "operation_error", "")
		return
	}
	d.Mark("parse_end")
}

// Finish and CloseResource use explicit completion state, not recovered values.
// When unwinding without a normal return the original panic is left untouched.
func (d *SearchDiagnostics) reason(ctx context.Context, returned, failed bool) string {
	if d != nil {
		d.mu.Lock()
		reason := d.contextReason
		d.mu.Unlock()
		if reason != "" {
			return reason
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "context_deadline"
	}
	if ctx.Err() == context.Canceled {
		return "context_cancelled"
	}
	if !returned {
		return "panic_unwind"
	}
	if failed {
		return "error_return"
	}
	return "normal_return"
}

func (d *SearchDiagnostics) Finish(ctx context.Context, returned, failed bool) {
	d.ObserveContext(ctx)
	reason := d.reason(ctx, returned, failed)
	status, class := "success", "none"
	if reason != "normal_return" {
		status, class = "failure", "operation_error"
	}
	d.event("handler_return", status, class, reason)
}

func (d *SearchDiagnostics) CloseResource(ctx context.Context, resource string, returned, failed bool, closeFn func() error) {
	if d == nil {
		_ = closeFn()
		return
	}
	reason := d.reason(ctx, returned, failed)
	d.event("close_reason", "success", "none", reason)
	d.step(ctx, resource+"_close", closeFn)
}

// Probe is diagnostic-only: false, errors and even a probe-local panic never
// become search results/errors. Business operations do not use this recovery.
func (d *SearchDiagnostics) Probe(probe func() (bool, error)) {
	if d == nil {
		return
	}
	status, class := "unknown", "probe_error"
	defer func() {
		if recover() != nil {
			status, class = "unknown", "probe_error"
		}
		d.event("results_visible_probe", status, class, "")
	}()
	visible, err := probe()
	if err == nil {
		status, class = "false", "none"
		if visible {
			status = "true"
		}
	}
}

// A one-shot, read-only DOM probe, not a readiness condition. Use raw CDP to
// avoid Rod's evaluation retry machinery. Only the observer has a 100ms budget;
// the original page/context and its existing 60s deadline are never changed.
// Selector drift or an expired page context yields false/unknown, not failure.
func (d *SearchDiagnostics) ProbeResults(page *rod.Page) {
	if d == nil {
		return
	}
	d.Probe(func() (bool, error) {
		ctx, cancel := context.WithTimeout(page.GetContext(), 100*time.Millisecond)
		defer cancel()
		result, err := (proto.RuntimeEvaluate{
			Expression:    `(() => { const e = document.querySelector('.feeds-container section.note-item'); if (!e) return false; const r = e.getBoundingClientRect(); const s = getComputedStyle(e); return r.width > 0 && r.height > 0 && s.display !== 'none' && s.visibility !== 'hidden'; })()`,
			ReturnByValue: true, Silent: true,
		}).Call(page.Context(ctx))
		if err != nil {
			return false, err
		}
		if result.ExceptionDetails != nil || result.Result == nil || result.Result.Type != proto.RuntimeRemoteObjectTypeBoolean {
			return false, context.Canceled // fixed classification; no remote exception text
		}
		return result.Result.Value.Bool(), nil
	})
}

// Provenance records only counts, never feed contents or arbitrary ModelType
// strings. New/unrecognized types intentionally collapse to unknown. Counts are
// absent (not zero) when extraction/parse did not produce a []Feed.
func (d *SearchDiagnostics) Provenance(source string, feeds, notes []Feed, parsed bool, states ...*FeedContainerState) {
	if d == nil {
		return
	}
	switch source {
	case "value", "_value", "none", "unknown":
	default:
		source = "unknown"
	}
	record := &SearchProvenance{ExtractionSource: source}
	if len(states) == 1 {
		record.FeedContainerState = states[0]
	}
	status, class := "failure", "operation_error"
	if parsed {
		raw, post := len(feeds), len(notes)
		record.RawExtractedFeedCount, record.PostOnlyNotesCount = &raw, &post
		record.ModelTypeCounts = map[string]int{}
		for _, feed := range feeds {
			kind := "unknown"
			switch feed.ModelType {
			case "note", "live_v2", "hot_query":
				kind = feed.ModelType
			}
			record.ModelTypeCounts[kind]++
		}
		status, class = "success", "none"
	}
	d.event("zero_result_provenance", status, class, "", record)
}
