package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel is the generic Webhook adapter: users customize the URL, method, request headers, and JSON template.
// Its existence means this feature needs no separate implementation for Slack / Mattermost / Discord / self-hosted systems -- ...
// All those platforms can be covered by one configurable template.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// Generic webhooks have no official limit; returning 0 means no rate limiting by default, left for users to set per peer capability.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// Masks url and headers: the destination address itself often carries a token, and custom headers usually hold auth credentials,
// Both show up in API echoes, so both must be blocked.
// The cost is that editing any one header requires re-filling the whole header set (masked values mean "keep original") --
// This trade-off is deliberate: rather ask users to fill it in once more than echo credentials back to the browser.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// The destination is the url. When changing the url, headers must be re-stated -- otherwise the original Authorization header ...
// It would be forwarded verbatim to the new address -- the main mask-bypass path.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate is the fallback request body when no template is filled in: a plain JSON structure,
// Covers the vast majority of self-hosted receivers that "take in one JSON record".
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData is the context exposed to user templates.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt is the delivery time of this attempt (RFC3339), recorded for the receiver.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel is a human-readable description of a status change, e.g. "pending -> fixed"; empty for non-status changes.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("Missing target URL")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("Invalid target URL: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("Unsupported method %s (supported: GET/POST/PUT/PATCH)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("Request-body template syntax error: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET carries no request body: stuffing content into query exceeds template capability and violates GET semantics,
	// So GET is only suitable for receivers of the "a hit fires the hook" kind.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// The template renders JSON in string form, converted here to json.RawMessage and sent as-is,
		// Avoid double-escaping that would wrap the user's carefully constructed structure into a JSON string.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("Request body template rendered result is not Valid JSON"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// Overriding is allowed, but apply it after headers so explicit configuration takes precedence.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// Generic Webhook does not truncate the body (the receiving end is the user's own service; size is determined by body_template),
	// So the whole batch counts as delivered.
	return len(m.Items), nil
}

// renderWebhookBody renders the request body with the user template (or the default template).
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("Request-body template syntax error: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("Failed to render request-body template: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate parses the template.
//
// missingkey=zero renders missing map keys as zero values instead of erroring -- but this file's context is structs,
// Its main effect is keeping range from erroring when .Items is empty. What genuinely needs guarding is .Items being nil.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs are helper functions exposed to templates.
var webhookTemplateFuncs = template.FuncMap{
	// json serializes any value to JSON.
	//
	// This function is a necessity, not a nice-to-have: without it, users can only write {{.Title}} direct interpolation,
	// ...but if the vulnerability title contains a quote or newline, the whole request body is no longer valid JSON -- the receiving end will
	// ...reject it, with the error pointing at "JSON parse failure", giving no hint that a quote in the title caused it.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons is used to embed a JSON fragment inside another JSON string value (adding one layer of string escaping).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// Strip the outer quotes: callers decide whether to add quotes themselves.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " -> " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
