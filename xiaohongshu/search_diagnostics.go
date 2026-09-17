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
)

type searchDiagnosticKey struct{}

type SearchDiagnostics struct {
	mu            sync.Mutex
	file          *os.File
	requestID     string
	start         time.Time
	contextReason string
}

type searchDiagnosticEvent struct {
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
		"page_close_start", "page_close_end", "browser_close_start", "browser_close_end", "close_reason":
		return true
	}
	return false
}

func (d *SearchDiagnostics) event(stage, status, errorClass, reason string) {
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
	data, err := json.Marshal(searchDiagnosticEvent{d.requestID, float64(time.Since(d.start).Nanoseconds()) / 1e6, stage, status, errorClass, reason})
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
