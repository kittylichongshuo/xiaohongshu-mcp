package xiaohongshu

import (
	"encoding/json"
	"github.com/ysmood/gson"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestTimelineOfflineDOMAndState(t *testing.T) {
	for _, tc := range []struct {
		name, feeds string
		dom         int
		container   bool
		value, back int
	}{
		{"populated", `{value:[1,2],_value:[3]}`, 18, true, 2, 1},
		{"empty", `{value:[],_value:[]}`, 0, false, 0, 0},
		{"missing", `undefined`, 0, false, -1, -1},
		{"nonarray", `{value:{},_value:'private-token'}`, 2, true, -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, err := exec.LookPath("node")
			if err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(map[string]any{"script": searchStateTimelineJS, "feeds": tc.feeds, "dom": tc.dom, "container": tc.container})
			cmd := exec.Command(node, "-e", `const vm=require('vm'),fs=require('fs');const i=JSON.parse(fs.readFileSync(0,'utf8'));const c=vm.createContext({document:{querySelectorAll(s){if(s!=='.feeds-container section.note-item')throw Error();return {length:i.dom}},querySelector(s){if(s!=='.feeds-container')throw Error();return i.container?{}:null}}});vm.runInContext('window={__INITIAL_STATE__:{search:{feeds:'+i.feeds+'}},location:{href:"https://example.invalid/search_result?keyword=private-query",pathname:"/search_result"}}',c);process.stdout.write(JSON.stringify(vm.runInContext(i.script,c,{timeout:1000})));`)
			cmd.Stdin = strings.NewReader(string(input))
			raw, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if json.Unmarshal(raw, &value) != nil {
				t.Fatal("invalid observation")
			}
			_, d, path := diagnosticTestStart(t)
			for _, phase := range []string{"after_navigation", "after_stable_wait", "after_result_state_wait", "before_extraction"} {
				d.timeline(phase, func() (gson.JSON, error) { return gson.New(value), nil })
			}
			events := timelineTestEvents(t, path)
			if len(events) != 4 {
				t.Fatal(events)
			}
			for _, e := range events {
				s := &e.SearchStateTimeline
				if e.Status != "success" || *s.DOMNoteCount != tc.dom || *s.ResultContainerPresent != tc.container {
					t.Fatal(e)
				}
				for j, p := range []*int{s.ValueCount, s.BackingValueCount} {
					want := []int{tc.value, tc.back}[j]
					if want < 0 {
						if p != nil {
							t.Fatal("fabricated zero")
						}
					} else if p == nil || *p != want {
						t.Fatal("wrong count")
					}
				}
			}
			log, _ := os.ReadFile(path)
			for _, secret := range []string{"private-", "keyword", "href", "title", "author", "cookie", "token", "HTML", "__INITIAL_STATE__"} {
				if strings.Contains(string(log), secret) {
					t.Fatal("unsafe log")
				}
			}
		})
	}
}

func TestTimelineObserverFailures(t *testing.T) {
	_, d, path := diagnosticTestStart(t)
	d.timeline("before_extraction", func() (gson.JSON, error) { panic("private-token") })
	d.timeline("after_navigation", func() (gson.JSON, error) { return gson.New(map[string]any{"dom_note_count": "private-body"}), nil })
	for _, e := range timelineTestEvents(t, path) {
		if e.Status != "unknown" || e.DOMNoteCount != nil {
			t.Fatal(e)
		}
	}
	var disabled *SearchDiagnostics
	disabled.timeline("before_extraction", func() (gson.JSON, error) { t.Fatal("disabled probe ran"); return gson.New(nil), nil })
	d.timeline("private-query", func() (gson.JSON, error) { t.Fatal("invalid phase ran"); return gson.New(nil), nil })
}

func timelineTestEvents(t *testing.T, path string) []struct {
	SearchStateTimeline
	Status string
} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []struct {
		SearchStateTimeline
		Status string
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event struct {
			SearchStateTimeline
			Status string
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			t.Fatal("invalid event")
		}
		events = append(events, event)
	}
	return events
}
