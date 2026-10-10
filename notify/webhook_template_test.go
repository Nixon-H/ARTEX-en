package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file pins down the **capability boundary** of the generic Webhook template.
//
// This is the only place in the whole package where "user-supplied strings get evaluated as code", so it must be explicit about what it can and
// ...what it must not do, pinning these properties down with tests -- otherwise someone may casually add a ... to the template context in the future
// methods, or adding a readFile to the FuncMap, would silently expand the capability surface while the diff looks
// Just a harmless little helper.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} can read fields and call methods). So the template context
// Anything that can reach **any** type with exported methods effectively exposes those methods to template authors.
// This feature's context is deliberately pure data (exported fields only, zero methods).
//
// If this fails: someone added methods to webhookTemplateData / webhookItem.
// Before deciding to allowlist it, think through whether that method could be used by templates to read things that should not be exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can call them, "+
				"Effectively exposes these methods to template authors", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks down the set of functions exposed to templates.
//
// Each extra function in FuncMap adds one capability. Currently only json / jsons, which serialize values
// into JSON fragments -- cannot read files, send requests, or execute commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Template function set changed: got %v, want %v. Before adding a function, confirm it does not expand the capability surface"+
			"(cannot read/write files, cannot initiate network requests, cannot execute commands)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in templates:
// Accessing something nonexistent must fail, not echo anything back; and the failure message must not leak internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("Accessing a nonexistent field should error")
	}
	// Errors must not contain real content from the template context (vulnerability titles/summaries).
	for _, leak := range []string{"SQL injection", "Param id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("Template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a mistyped template is a configuration error; retries will not heal it.
// If judged retryable, one bad template would make every delivery burn three backoff rounds for nothing.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("Template syntax errors should be caught at save time")
	}
	// Even when validation is bypassed and delivery proceeds directly, it must be judged a permanent failure rather than retried repeatedly.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("Bad template should be permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that "template rendering output must be valid JSON".
// It incidentally blocks usages like "generating plaintext with templates to trigger other protocols".
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// Valid templates pass.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("Valid template should pass validation: %v", err)
	}
	// Rendered non-JSON output must be rejected (instead of being sent as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("Rendering non-JSON should be permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "Valid JSON") {
		t.Errorf("Error message should say it is a JSON problem, got %v", err)
	}
}
