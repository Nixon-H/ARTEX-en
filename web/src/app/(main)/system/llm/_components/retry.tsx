"use client";

// Shared pieces for the LLM retry config: each of the five layers has its own "attempts + interval".
//
// The five layers from innermost to outermost: connect (SDK) → empty response (SDK) → same-provider safety window → pool circuit breaker → intent rerun.
// The first three layers follow the endpoint, so each model config can override the global defaults; the last two are process-level with a single global copy.
//
// All inputs share the same "leave blank = unconfigured" semantics, matching backend db.RetryRule:
//   retry count   empty/0 = use the built-in default | -1 = disable retries at this layer | >0 = Use this count
//   interval   empty/0 = keep this layer's exponential backoff | >0 = switch to this fixed millisecond interval

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** Where this retry layer happens and who executes it */
  where: string;
  /** Which errors reach this layer — down to specific status codes, no guessing */
  trigger: string;
  /** Lookalike errors that do 【not】 go through this layer, so you don't fill it in and mistake silence for a bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default when attempts is left blank, used for the placeholder */
  defAttempts: number;
  /** Default policy when interval is left blank, used for the placeholder */
  defInterval: string;
  /** Meaning of entering -1 for attempts */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connection retry",
    where: "SDK · before receiving 200",
    trigger:
      "Not connected or no 200 yet: connection resets / read-write timeouts / DNS failures and other network-layer errors, plus HTTP 408, 429, 500, 502, 503, 504.",
    skips: "All other status codes (400 / 401 / 403 / 404 / 413 / 422, etc.) are deterministic rejections — resending fails the same way, so they are thrown immediately.",
    desc: "Resends the same request as-is. Once the stream has started (200 already received), a mid-stream drop no longer belongs to this layer.",
    attemptsLabel: "Retry attempts",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = no retries at all, failures are thrown immediately",
  },
  empty: {
    title: "Empty-response retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200 with a normal stop finish_reason, yet not a single content block in the whole response — gateway empty frames, dropped thinking fields, and sampling hiccups all look like this.",
    skips: "No content because of max_tokens truncation does not count (that needs a higher output cap; resending just hits the same wall).",
    desc: "Resends the entire prompt, so it is expensive on long contexts — keep the count small.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = hand empty responses back as-is",
  },
  stream: {
    title: "Same-provider safety-window retry",
    where: "This project · before any output is delivered",
    trigger:
      "The problem shows up after the stream is established (200 received): mid-stream disconnects, provider overload, in-stream 429 / 5xx error events — and not a single token has been handed to the caller yet.",
    skips:
      "Quota exhaustion (402 / insufficient_quota, handled by failover to another config), over-long context (413 / context length, handled by compression), and deterministic 400 / 401 / 403 / 404 / 422 rejections are not retried.",
    desc: "Replays the same request on the same config. Since no output has been delivered yet, replaying cannot duplicate model output or tool execution.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (capped at 4s)",
    offHint: "-1 = hand broken streams directly to the outer intent rerun",
  },
  breaker: {
    title: "Pool circuit breaker",
    where: "This project · process-level, single global copy",
    trigger:
      "Trips the circuit breaker once transient failures (429, 5xx, network errors) accumulate to the threshold; deterministic failures — insufficient balance (402), invalid Key (401 / 403), missing model (404) — ignore the threshold and trip on the first occurrence.",
    skips: "One success resets the count, so an occasionally flaky config never slowly accumulates its way into a trip.",
    desc: "After tripping, the config enters cooldown, during which polling skips it outright. State is persisted to the DB and survives restarts.",
    attemptsLabel: "Consecutive failures to trip",
    defAttempts: 3,
    defInterval: "1min→5min→30min ladder",
    offHint: "-1 = transient failures never trip (deterministic failures still trip)",
  },
  intent: {
    title: "Intent rerun",
    where: "This project · process-level, single global copy",
    trigger:
      "The earlier layers didn't catch it: the worker ends in model_error — inner retries all exhausted, or the stream broke after output delivery had already started (replay is unsafe then, so the whole thing must rerun).",
    skips: "Quota exhaustion is already handled by pool failover, not rerun here; when a task is paused / terminated / entering wind-down, yield immediately instead of consuming backoff time.",
    desc: "Reruns the whole intent from scratch. It is the outermost layer, so one rerun multiplies the attempt counts of all inner layers again.",
    attemptsLabel: "Rerun attempts",
    defAttempts: 2,
    defInterval: "Fixed 3s",
    offHint: "-1 = no rerun; the intent is marked blocked directly",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Milliseconds in human words, only echoed next to the input so nobody has to count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled numeric input: empty string ↔ 0; intermediate states ("-", "1e") stay local as-is without bothering the parent. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // Catches up when the parent swaps in a whole new set of values (policy loaded, config switched); never reached while typing,
  // because by then value already equals the parsed local text.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** The two knobs of one retry layer. idPrefix keeps label htmlFor intact when it appears multiple times on the same page. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = compact version in the config drawer: skip the expanded notes, keep only the "which errors reach this layer" line */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* Which errors reach this layer, down to status codes — if the knobs seem to do nothing, the error most likely never lands on this layer. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Triggers</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Skips this layer</span>: {meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`Default ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            Interval ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="Default backoff"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `Fixed ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">Blank = use defaults; {meta.offHint}.</p>}
    </div>
  );
}

/** The three-layer override in the model-config drawer (the three layers that follow the endpoint). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">Retry override</Label>
        <p className="text-muted-foreground text-xs">
          Applies to this config only, overriding the global defaults under "Retry & backoff". Leaving a cell blank follows the global value; -1 for attempts disables retries at this layer;
          A filled interval replaces exponential backoff with a fixed interval. The circuit breaker and intent rerun are process-level and can only be tuned on the global page.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** "Retry & backoff" tab: global defaults for the five layers. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`Failed to read retry policy: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // The backend clamps out-of-range values into range and returns them; refresh from the returned values so what you see is what's stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved, effective immediately (the currently running round of calls still uses the old parameters)");
    } catch (e) {
      toast.error(`save failed: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> Reading retry policy…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        A failed model call passes through the five retry layers in order, innermost first:
        <span className="text-foreground"> Connect → empty response → same-provider safety window → pool circuit breaker → intent rerun</span>
        . An outer layer only runs after inner ones are exhausted, so the counts
        <span className="text-foreground">multiply</span>
        — max out every layer and one blip can burn dozens of requests.
        Leaving everything blank means the current defaults, identical to the behavior before this page existed. The first three layers can be overridden per model config.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          Save
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          Restore all defaults
        </Button>
      </div>
    </div>
  );
}
