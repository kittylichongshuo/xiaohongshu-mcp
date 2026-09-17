package xiaohongshu

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ysmood/gson"
)

// Execute the actual extraction JS against synthetic in-memory state in Node.
// No browser, MCP, URL navigation or network API is used by this harness.
func runContainerJS(t *testing.T, setup string, enabled bool) gson.JSON {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("offline JS unit tests require node")
	}
	f, err := parser.ParseFile(token.NewFileSet(), "search.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var script string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "MustEval" {
			return true
		}
		if len(call.Args) > 0 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok {
				script, _ = strconv.Unquote(lit.Value)
			}
		}
		return true
	})
	if script == "" {
		t.Fatal("extraction script not found")
	}
	encoded, _ := json.Marshal(map[string]any{"script": script, "setup": setup, "enabled": enabled})
	cmd := exec.Command(node, "-e", `const fs=require('fs');const vm=require('vm');const i=JSON.parse(fs.readFileSync(0,'utf8'));const c=vm.createContext({});vm.runInContext('globalThis.window={};'+i.setup,c,{timeout:1000});const out=vm.runInContext('('+i.script+')('+JSON.stringify(i.enabled)+')',c,{timeout:1000});process.stdout.write(JSON.stringify(out));`)
	cmd.Stdin = strings.NewReader(string(encoded))
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("offline JS evaluation failed", err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return gson.New(value)
}

func TestDualStateContainerCases(t *testing.T) {
	for _, tc := range []struct {
		name, setup, source, data                        string
		valuePresent, valueArray, backPresent, backArray bool
		valueCount, backCount, selected                  int
	}{
		{"value only", `window.__INITIAL_STATE__={search:{feeds:{value:[{modelType:'note'}]}}};`, "value", `[{"modelType":"note"}]`, true, true, false, false, 1, -1, 1},
		{"empty value populated backing", `window.__INITIAL_STATE__={search:{feeds:{value:[],_value:[{modelType:'note'},{modelType:'note'}]}}};`, "value", `[]`, true, true, true, true, 0, 2, 0},
		{"backing only", `window.__INITIAL_STATE__={search:{feeds:{_value:[{modelType:'note'}]}}};`, "_value", `[{"modelType":"note"}]`, false, false, true, true, -1, 1, 1},
		{"both empty", `window.__INITIAL_STATE__={search:{feeds:{value:[],_value:[]}}};`, "value", `[]`, true, true, true, true, 0, 0, 0},
		{"neither present", `window.__INITIAL_STATE__={search:{feeds:{}}};`, "none", ``, false, false, false, false, -1, -1, -1},
		{"non arrays", `window.__INITIAL_STATE__={search:{feeds:{value:{safe:true},_value:'synthetic'}}};`, "value", `{"safe":true}`, true, false, true, false, -1, -1, -1},
		{"null value with backing", `window.__INITIAL_STATE__={search:{feeds:{value:null,_value:[1]}}};`, "value", ``, true, false, true, true, -1, 1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled := runContainerJS(t, tc.setup, true)
			disabled := runContainerJS(t, tc.setup, false)
			if enabled.Get("data").Str() != tc.data || disabled.Str() != tc.data {
				t.Fatal("extraction changed")
			}
			state := decodeFeedContainerState(enabled.Get("container_state"))
			if state.SelectedSource != tc.source {
				t.Fatal(state)
			}
			if state.InitialStateSearchPresent == nil || !*state.InitialStateSearchPresent || state.FeedsContainerPresent == nil || !*state.FeedsContainerPresent {
				t.Fatal("presence missing")
			}
			bools := []*bool{state.ValuePresent, state.ValueIsArray, state.BackingValuePresent, state.BackingValueIsArray}
			wants := []bool{tc.valuePresent, tc.valueArray, tc.backPresent, tc.backArray}
			for i, b := range bools {
				if b == nil || *b != wants[i] {
					t.Fatal("wrong boolean", i)
				}
			}
			counts := []*int{state.ValueCount, state.BackingValueCount, state.SelectedCount}
			for i, want := range []int{tc.valueCount, tc.backCount, tc.selected} {
				if want < 0 {
					if counts[i] != nil {
						t.Fatal("fabricated zero", i)
					}
				} else if counts[i] == nil || *counts[i] != want {
					t.Fatal("incorrect count", i)
				}
			}
			if tc.name == "empty value populated backing" {
				_, d, path := diagnosticTestStart(t)
				d.Provenance(enabled.Get("source").Str(), []Feed{}, []Feed{}, true, state)
				event := diagnosticTestEvents(t, path)[0]
				if *event.RawExtractedFeedCount != 0 || *event.PostOnlyNotesCount != 0 || event.ExtractionSource != "value" || len(event.ModelTypeCounts) != 0 {
					t.Fatal("existing provenance changed")
				}
				raw, _ := os.ReadFile(path)
				t.Log(string(raw))
			}
		})
	}
}

func TestDualStateMissingContainers(t *testing.T) {
	for _, setup := range []string{``, `window.__INITIAL_STATE__={};`, `window.__INITIAL_STATE__={search:{}};`} {
		result := runContainerJS(t, setup, true)
		state := decodeFeedContainerState(result.Get("container_state"))
		if state.SelectedSource != "none" || state.SelectedCount != nil || state.ValueCount != nil || state.BackingValueCount != nil {
			t.Fatal(state)
		}
		if result.Get("data").Str() != runContainerJS(t, setup, false).Str() {
			t.Fatal("missing state changed extraction")
		}
	}
}

func TestDualStateInspectionFailureIsNoOp(t *testing.T) {
	// Access to the unselected backing container throws only during diagnostics.
	setup := `window.__INITIAL_STATE__={search:{feeds:{value:[]}}};Object.defineProperty(window.__INITIAL_STATE__.search.feeds,'_value',{get(){throw new Error('private-token')}});`
	enabled := runContainerJS(t, setup, true)
	if enabled.Get("data").Str() != runContainerJS(t, setup, false).Str() {
		t.Fatal("observer changed result")
	}
	if decodeFeedContainerState(enabled.Get("container_state")).SelectedSource != "unknown" {
		t.Fatal("failure not unknown")
	}
}

func TestDualStateMetadataSafetyAndWriteFailure(t *testing.T) {
	for _, value := range []any{nil, "private-token", map[string]any{"selected_source": "private-account"}, map[string]any{"selected_source": "value", "value_count": "private-body"}} {
		if decodeFeedContainerState(gson.New(value)).SelectedSource != "unknown" {
			t.Fatal("unsafe metadata accepted")
		}
	}
	result := runContainerJS(t, `window.__INITIAL_STATE__={search:{feeds:{value:[{modelType:'note',id:'private-id',title:'private-title',author:'private-author',body:'private-body',cookie:'private-cookie',token:'private-token',xsec_token:'private-xsec'}],_value:[]}}};`, true)
	state := decodeFeedContainerState(result.Get("container_state"))
	var feeds []Feed
	if err := json.Unmarshal([]byte(result.Get("data").Str()), &feeds); err != nil {
		t.Fatal(err)
	}
	notes := onlyNotes(feeds)
	_, d, path := diagnosticTestStart(t)
	d.Provenance("value", feeds, notes, true, state)
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "private-") || strings.Contains(string(raw), result.Get("data").Str()) {
		t.Fatal("payload leaked")
	}
	_ = d.file.Close()
	d.Provenance("value", feeds, notes, true, state)
	if !reflect.DeepEqual(notes, onlyNotes(feeds)) {
		t.Fatal("sink failure changed output")
	}
}
