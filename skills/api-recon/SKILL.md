---
name: api-recon
description: Invoke this skill when collecting website API endpoints.
---

# API Recon (Frontend Interface Reconnaissance)

Under the **authorized** premise, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature trigger points** (tabs, popups, table operations, etc.).

---

## Boundaries and Prohibitions (Required Reading for Agents; Violations Are Out of Scope)

This skill is **only for API / parameter-surface reconnaissance**, not for vulnerability discovery or exploitation stages.

### Task Boundaries

| scope | Allow | deny |
|---|---|---|
| **Target** | enumerate path, method, parameter, route, UI trigger point | SQLi/XSS/broken access control/brute force/fuzz Vulnerability, request-tampering attacks, destructive operations |
| **auth** | Hook + stub/mock bypass**client**Login gate | asking the user for, or guessing, account credentials; attempting real login-form submission |
| **runtime** | Without credentials hook Interface, use mock mock responses let (the SPA enter the post-login shell) SPA reach the post-login shell | flows requiring a real backend session to proceed |

### Credential-Free Dynamic Analysis (Phase 3 Default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** bootstrap interfaces such as login, permissions, and menus;
2. Return mock bodies with **correct structure, successful business codes, and nullable data** for business query interfaces;
3. Enable the frontend to still render post-login pages with no backend or in a 401 environment, thereby triggering more XHR/fetch/WebSocket traffic;
4. **Empty data, blank tables, and placeholder UI are all expected** -- do not switch to real login or vulnerability testing because of them.

**In one sentence**: use mocks to prop open frontend routes and component mounting, and **record only outbound requests**; what the backend returns does not matter -- what matters is **which interfaces the frontend will still call**.

### Strict Process Prohibitions

| deny | Alternative approach |
|---|---|
| Phase 1 before completion grep/curl/Read main entry `index-*.js` extract API path | run `OUTDIR/harvest_static.py` |
| hand-written `extract_apis.py` alternatives such as harvest Script for | edit `OUTDIR/harvest_static.py` re-run after editing |
| same grep/command failure ≥2 Still repeating after repeated failures | switch strategy: read tool_logs, edit harvest, look up reference |
| skip the gates A/B, run directly `scripts/` original version | copy to OUTDIR and modify for the target |
| real username/password, OTP, OAuth auth such as | stub/mock (see above) |
| under the pretext of"to get real data"Skipped under the pretext of ... stub, Perform unauthorized-access testing/injection testing | record only outbound, belongs to recon boundary |
| Delete, exporting sensitive data, bulk writes and other irreversible operations | coverage Clicks behave the same |
| incomplete runtime + dynamic enumeration, claiming full page and API coverage | see"definition of done"or annotate the limitations |
| incomplete parameter-trigger matrix + diff, claiming full parameter coverage | Phase 3b matrix + Phase 5 diff |
| Using a single sample runtime infer required-ness from samples/optional | multi-sample diff or validation rules/reverse-infer from errors |

---

## Two-Layer Model and Run Modes

| layer | output | upper limit |
|---|---|---|
| **static** (JS bundle) | complete set endpoint path, route draft, field candidates at request-building points | no HTTP method; Parameters must ... Phase 1b; miss runtime-built URL |
| **runtime** (Live session) | method + body + response + dynamic URL + WS/SSE; multi-sample diff fill in missing parameters | pages only send requests after actually rendering; a single sample can't determine required vs optional/optional |

| run mode | engine | applies to |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response body, WS/SSE, Reproducible batch runs |
| **coverage** | browser + `preload.js` | click Tab/dialog/table, Deeper feature coverage |
| **both** | first depth then coverage | Most complete, takes the longest |

**Parameter methodology** (no generic script): use harvest/regex for paths; use **anchor window-expansion + UI binding chains + multi-sample diff + error-based inference** for parameters (see section J of [reference.md](reference.md) for grep recipes).

---

## Definition of Done

Recon may only be claimed complete when all of the following are satisfied:

- [ ] **Static**: Phase 1 harvest produces `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least depth or coverage; coverage/both require **working hooks + dynamic enumeration loop**
- [ ] **Shell entry**: visiting a business path does not land on `/login` (watch out for hash routes)
- [ ] **Parameters**: coverage/both complete the parameter-trigger matrix + `param_samples.json`; Phase 5 merges `params_merged.json`
- [ ] **Depth** (if module pages are blank): restore the permission tree in Phase 4 and re-run until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Deliverables**: Phase 5 outputs complete (see the Phase 5 output table); `insert_assets` writes service and endpoint assets

---

## Scripts And Gates

`scripts/` are reference templates only; **do not** run the originals directly and treat the result as final.

**Rule**: read first, adapt to the target, write to `OUTDIR` (e.g. `recon/`), record in `CHANGES.md`; if it does not fit, rewrite per the methodology, borrowing only the structure.

| gate | when | reference script -> OUTDIR copy | common must-change items |
|---|---|---|---|
| **A (static)** | Phase 0 after, **first time**run harvest/spider before | `harvest_static.py` / `spider_mpa.py` | **default for most sites regex Can run directly**; only manifest/Change only on dialect mismatch endpoint regex, webpack/Vite `publicPath`, MPA exclude/cookie |
| **B (runtime)** | Phase 2 after, run depth/coverage before | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage key, neutralize success values, stubs, login regex, api prefix, hash/history |

**Mandatory SPA order** (non-interchangeable; Phase numbers take precedence over 'explore before scripting'):

| step | must | deny |
|---|---|---|
| Phase 0 after completion | next command Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read main entry `index-*.js` (usually >500KB) |
| gate A | copy the script -> tweak as needed -> **run immediately** | Manually extract first API then decide whether to harvest |
| Phase 1 before completion | `wc -l` Verify outputs; 404 edit harvest Retry | hand-written extract script; for URLs not yet downloaded URL repeatedly grep |
| Phase 1b start | grep only `OUTDIR/js/*.js` | use the main bundle instead of harvest |

- Copy `harvest_static.py`, (optionally) change regex, **run immediately**
- curl the main bundle, grep multiple times, write a temporary extract, harvest only at the end
- **MPA**: after Phase 0, the next Bash command is `python3 OUTDIR/spider_mpa.py ...`

---

## Tooling And Output Constraints

| constraint | description |
|---|---|
| large files | >100KB 's `index-*.js` **deny** Read/grep load into context; use OUTDIR batch-process with scripts |
| grep output | must `\| head -20` or `-m 5`; keep only (the path summary) in the conversation path summary, Do not paste ... bundle snippet |
| validate | use `wc -l`, `ls \| wc -l`; don't Read Entire directory |
| regex initial probe | optional, ≤1 time(s), only ≤50KB small chunk or HTML; Authoritative static results follow harvest harvest shall prevail |
| reference | recipe/template/see (reference) for troubleshooting [reference.md](reference.md), Do not duplicate ... inline full text |

---

## Execution Roadmap

```
Phase 0 classification + OUTDIR
  Gate A, Phase 1 harvest (run immediately)
  Phase 1b parameter reverse-engineering
  -> Phase 2 three auth gates -> config.json
  -> Gate B -> Phase 3 runtime + parameter matrix
  -> Phase 4 permission tree (if needed) -> re-run Phase 3
  -> Phase 5 merged report + insert_assets: bulk-insert all discovered services and endpoint API assets; no discovered asset may be omitted at insert time
```

Check off in order; **do not enter the next Phase until the previous one is complete**.

1. [ ] **Phase 0**: probe SPA/MPA; create `OUTDIR` -> [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy scripts -> harvest **immediately** -> verify with `wc -l` -> [Phase 1](#phase-1--static-analysis) (static)
3. [ ] **Phase 1b**: anchor window-expansion + binding layer -> `param_candidates.json` -> [Phase 1b](#phase-1b--parameter-reverse-engineering) (parameter reverse-engineering)
4. [ ] **Phase 2**: three auth gates -> `config.json` -> [Phase 2](#phase-2--three-auth-gates) (three auth gates)
5. [ ] **Gate B**: adjust runtime scripts -> [Phase 3](#phase-3--runtime) (runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm shell entry; parameter trigger matrix -> `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree -> patch stubs -> re-run Phase 3 -> [Phase 4](#phase-4--permission-tree-reconstruction) (permission-tree restoration)
8. [ ] **Phase 5**: merged outputs + report + `insert_assets` -> [Phase 5](#phase-5--merge-and-report) (merge and report)

---

## Phase 0 — Classification

Fetch the entry HTML and **create `OUTDIR`** (do not modify `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunk -> Phases 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle -> after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Produces `forms.txt`, `links.txt`, `api_inline.txt`. For an SPA, if forms ≈ 0 -> switch to Phase 1.

---

## Phase 1 — Static Analysis

Comply with [Scripts and Gates](#scripts-and-gates) (scripts and gates) · [Tooling And Output Constraints](#tooling-and-output-constraints) (tool and output constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML scripts -> webpack/Vite manifest -> download all lazy chunks -> produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk count vs manifest: on 404, fix harvest and retry; do not manually curl chunks one by one
- Too few entries in `api_static.txt` -> relax the endpoint regex inside OUTDIR and re-run (see reference)

### Phase 1b — Parameter Reverse-Engineering

paths come from Phase 1; parameter fields require a separate recon. For grep rules see [Tooling And Output Constraints](#tooling-and-output-constraints).

**Done criteria**: for key APIs be able to answer -- field names, transport location, inferred types, required or not, sample values, confidence.

#### 1b.0 — Transport Shape

| shape | Where the parameters live | look at static (sources) first |
|---|---|---|
| REST JSON | body + query | path beside the anchor `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql template, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| file upload | multipart | `FormData.append` |
| path params | `/user/:id` | route table + `useParams` / `$route.params` |
| encrypt/signature | pack into `sign`/`data` | Hook Encryption-function input parameters (reference D section) |

output: annotate per endpoint `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor Expansion

Use a known path as the anchor and widen the window to find the packet-building object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| wrapper layer | Parameter clues |
|---|---|
| axios instance | `data` / `params` |
| unified request | interceptor injects global fields |
| OpenAPI client | generate method signature |
| React Query / SWR | hook second parameter |
| Vue composable | composable input params |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`：`{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding Layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| binding source | technique |
|---|---|
| form submit | follow submit -> transform -> API |
| table search | `getFieldsValue()` -> `params` |
| route | `:id` / `?tab=` |
| interceptor | global `tenantId`, pagination, sign |
| enumerate select | `options` -> API Enum value |

In the DevTools call stack, trace up from `fetch`/`XHR.send` to the packet-building function.

#### 1b.3 — Three Request-Building Questions (Not The Phase 2 Auth Gates)

| ask | what to answer |
|---|---|
| **assemble** | payload where build, transform trace |
| **validate** | required, pattern, enum |
| **transport** | path / query / body / multipart / header |

At the interceptor gate (Phase 2), also note globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Handoff To Phase 3

Candidate fields come from the static/binding layers; **required/optional/conditional dependencies** require the Phase 3 parameter matrix + diff + Phase 5 error back-inference.

---

## Phase 2 — Three Auth Gates

Grep in `OUTDIR/js/` (with `head`) and write to `config.json` (recipe in reference):

| gate | question | keywords |
|---|---|---|
| **render gate** | how to tell if logged in? | `isLogin`, `getToken`, Cookie/localStorage |
| **interceptor gate** | what triggers redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **content gate** | menu/where permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Never treat a localStorage key name as a credential -- confirm from the chunk/request chain.

**Exit = Gate B**: land the conclusions in `config.json` and update `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API Observation (Optional)

Use the `preload.js` inside OUTDIR to confirm session key names, Authorization, and nested API URLs:

| config | output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers observe |
| `extractUrlsFromResponse: true` | sub-APIs inside the response API |
| `observe.storageReads/cookieReads: true` | backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Each coverage round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Must have passed gate B; comply with [Boundaries & Prohibitions](#boundaries-and-prohibitions-required-reading-for-agents-violations-are-out-of-scope) (boundaries and prohibitions -- mandatory for agents, violations count as out-of-scope) and the no-credential mock policy.

`config.json` settings `"runtimeMode": "depth" | "coverage" | "both"` (See the template reference).

### Hook And Stub (Shared By Depth And Coverage)

| layer | scope | purpose |
|---|---|---|
| L1 exact | auth/permission/bootstrap stub | get past first-screen auth |
| L2 negative correction | all JSON response | Not-logged-in code -> Success |
| L3 fallback | missed L1 's `/api` etc. | empty success body, stretch UI |

- **depth**: fake auth + `forward` to rewrite business codes + `stubs`; walk through `routes` (hash/history); produce `runtime_api.json`
- **coverage**: **document-start** injection of `preload.js` (via CDP `addScriptToEvaluateOnNewDocument` or a userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; business paths don't bounce back to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — Dynamic Coverage Enumeration (Required)

1. Main nav/sidebar -- click each item, wait 1–3s for network
2. Tab — `role=tab`、`.ant-tabs-tab`
3. Tables -- view/edit/details on the first row
4. Toolbar -- export, filter, create (**avoid irreversible deletion**)
5. On entering each module -- merge APIs/routes
6. SPA -- controlled `pushState` for paths not covered in `routes.txt` (forbidden in MPAs)

**Parameter trigger matrix** (required): record once per operation type in each module, **diff multiple samples**:

| Actions | usually extra parameters |
|---|---|
| First screen of the list | pagination + default filters |
| Click search | keyword, filter |
| advanced filters | more optional |
| New/Edit | complete entity |
| bulk/export/sorting | `ids[]`, `exportType`, `sortField` |

**Outbound body/headers under stubs are still real** -- go by the request. Record -> `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**：`neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — Permission Tree Reconstruction

**Trigger**: blank module pages / only bootstrap per route (e.g. locale) -> content gate not passed.

| symptom | meaning |
|---|---|
| shell loaded successfully | render gate + interceptor gate passed |
| Missing sidebar items/Clicking empty areas | stub shape or incomplete permission codes |
| per route API identical per route and very few | `v-if permission` failed |
| `routes.txt` far fewer than bundle | must be completed from auth Complete from modules |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) -> `getResultTree` -> `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

Stub checks: outer `response_code` consistent with the interceptor gate; flat codes aligned with the tree; `routes` covering all links in `route_map`.

After updating `config.json`, **re-run Phase 3**. For large SPAs, tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference, sections A3/I).

---

## Phase 5 — Merge And Report

### Deliverables Table

| file | phase | content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | static bundle and path |
| `param_candidates.json` | 1b | static parameter field candidates |
| `config.json` | 2 | three gates + runtime config |
| `runtime_api.json` | 3a | depth detailed recording (including WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | multi-sample, Click logs, detail |
| `route_map.json` etc. | 4 | Permission-tree intermediate file (if executed) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | route, API, params, Feature, limitations |
| **insert_assets** | 5 | write all services (as assets), write endpoint assets into the asset library |

### 5b — Parameter Merging

Diff from `param_samples.json`; **no generic merge script**. For confidence rules see reference J7 (high/medium/low/pending-trigger).

### 5c — Error-Based Inference

Within the authorized scope, incomplete requests may be sent to read 400s (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Watch for the `data` wrapper, `variables`, and pre-encryption `bizData`.

The report must state: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, and a `CHANGES.md` summary relative to the reference scripts.

Suggested structure for `site_map.json`:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

For more fields and grep recipes, see [reference.md](reference.md).

---

## General Notes

- **Framework-agnostic**: same approach for webpack/Vite/Angular lazy load
- **Transports**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions not fully enumerable
- **Blind spots**: JSVMP, WASM, strong HMAC/mTLS validation -> static-only + note the limitations
- **Parameter blind spots**: conditional linkage, hidden params, WASM packet-building -> "pending-trigger" / "unreachable"
- **Static analysis is the safety net**: when runtime is blocked, static analysis can still enumerate endpoints

---

## Additional Resources

- Grep recipes, `config.json` template, troubleshooting, Hooks, parameter-reversing section J, site_map template: **[reference.md](reference.md)**
- See the [Scripts and gates](#scripts-and-gates) table for reference script paths
