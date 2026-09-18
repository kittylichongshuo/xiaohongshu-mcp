package xiaohongshu

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// Only scalar observations are returned. No card content or URL is read.
const searchStateTimelineJS = `(() => {
 const feeds = window.__INITIAL_STATE__?.search?.feeds;
 const value = feeds?.value;
 const backing = feeds?._value;
 return {
  dom_note_count: document.querySelectorAll('.feeds-container section.note-item').length,
  value_count: Array.isArray(value) ? value.length : null,
  _value_count: Array.isArray(backing) ? backing.length : null,
  result_container_present: document.querySelector('.feeds-container') !== null
 };
})()`

type SearchStateTimeline struct {
	Phase                  string `json:"phase"`
	DOMNoteCount           *int   `json:"dom_note_count,omitempty"`
	ValueCount             *int   `json:"value_count,omitempty"`
	BackingValueCount      *int   `json:"_value_count,omitempty"`
	ResultContainerPresent *bool  `json:"result_container_present,omitempty"`
}

// Recovery is restricted to the observer callback; business panics never pass
// through here. A malformed/failed observation is unknown, never fabricated zero.
func (d *SearchDiagnostics) timeline(phase string, probe func() (gson.JSON, error)) {
	if d == nil {
		return
	}
	switch phase {
	case "after_navigation", "after_stable_wait", "after_result_state_wait", "before_extraction":
	default:
		return
	}
	record := &SearchStateTimeline{Phase: phase}
	status, class := "unknown", "probe_error"
	defer func() {
		if recover() != nil {
			record = &SearchStateTimeline{Phase: phase}
			status, class = "unknown", "probe_error"
		}
		d.writeEvent(struct {
			*SearchStateTimeline
			searchDiagnosticEvent
		}{record, searchDiagnosticEvent{RequestID: d.requestID, ElapsedMS: float64(time.Since(d.start).Nanoseconds()) / 1e6, Stage: "search_state_timeline", Status: status, ErrorClass: class}})
	}()
	value, err := probe()
	if err != nil {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	var parsed SearchStateTimeline
	if json.Unmarshal(raw, &parsed) != nil || parsed.DOMNoteCount == nil || parsed.ResultContainerPresent == nil {
		return
	}
	for _, count := range []*int{parsed.DOMNoteCount, parsed.ValueCount, parsed.BackingValueCount} {
		if count != nil && *count < 0 {
			return
		}
	}
	parsed.Phase = phase
	record = &parsed
	status, class = "success", "none"
}

// One raw CDP evaluation, no Rod retry, polling or readiness condition. The
// child observation budget does not change/cancel the search page's deadline.
func (d *SearchDiagnostics) ProbeTimeline(page *rod.Page, phase string) {
	if d == nil {
		return
	}
	d.timeline(phase, func() (gson.JSON, error) {
		ctx, cancel := context.WithTimeout(page.GetContext(), 100*time.Millisecond)
		defer cancel()
		result, err := (proto.RuntimeEvaluate{Expression: searchStateTimelineJS, ReturnByValue: true, Silent: true}).Call(page.Context(ctx))
		if err != nil {
			return gson.New(nil), err
		}
		if result.ExceptionDetails != nil || result.Result == nil || result.Result.Type != proto.RuntimeRemoteObjectTypeObject {
			return gson.New(nil), context.Canceled
		}
		return result.Result.Value, nil
	})
}
