# api-recon — Reference Manual

Grep recipes, `config.json` templates, and troubleshooting. Run all greps against the `js/` directory. When a bundle is a single line, you may first run `js-beautify` or `sed 's/}/}\n/g'`; usually a raw grep with a context window suffices.

## Script Reference

All files under `scripts/` are **reference templates** and must be adapted to the target site before execution. Typical change points:

| script | common items needing adjustment |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parse, micro-frontend publicPath, Retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success values, stub Matching rules and ... body structure, routes source, WS record, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether enabled L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` destructive links, cookie, depth/max, same-domain filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parse, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field name |
| `config.json` | single entry point for all site-specific parameters above |

Place adjusted files in the task working directory (e.g. `recon/`), and note the specific changes relative to the reference scripts in the report.

---

## A. Three Reverse-Engineering Gates

### A1. Render Gate — How To Determine Login State

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Trace the chain `isLogin = f(getUser())` -> `getUser = decode(storage.read(KEY))` to determine the **storage key**, **container** (Cookie vs localStorage), and **encoding**:

| encoding | config Spoofing method |
|---|---|
| Plaintext string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | Unsigned/`alg:none` JWT, or bundle Signed with an in-bundle key |
| encrypt (SM2/AES/RSA) | find hardcoded keys; Render gate only needs a decodable blob blob can then forge; otherwise fall back to static handling |

-> Write into `cookies` / `localStorage`.

### A2. Interceptor Gate — What Triggers Redirects To /login

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(not logged in|please log in again|Login has expired|unauthorized|Login expired|authorization|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine the **field names**, **success values** (usually `0` or `200`), and the **failure values that trigger the redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

-> Write into `neutralize.fields` + `neutralize.success`.

### A3. Content Gate — Where Do Menus And Permissions Come From

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (typical enterprise admin backend):

| API | typical payload | consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | route guard, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | Sidebar menu rendering |
| bundle in `userRouteAuth` | `{ CODE: { url, name? } }` | code -> frontend path |
| bundle in `routeMap` | `{ KEY: { name, link } }` | Alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm: how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): build a permissive payload -> `stubs`.

**Full permission-tree reconstruction** (large sites where the sidebar/submodules are still blank): see **section I**.

---

## B. config.json Template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field notes:
- `runtimeMode`：`depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` prefixes: `b64json:` -> base64(JSON); `json:` -> raw JSON; no prefix -> literal value
- `forward: true` forwards the real request and rewrites the code field; `false` means fully offline stub
- `mockTier`: preload tiers enabled in coverage mode, e.g. `L1+L2`, `L1+L2+L3`
- `routes` come from `routes.txt`; after forging the menu the harness automatically appends `<a href>`
- `captureResponses` / `recordWs` only take effect in depth mode
- `waitUntil`: for large SPAs use `domcontentloaded` to avoid hanging on `networkidle2`
- `routeTimeout`: per-route `page.goto` timeout (milliseconds)
- `proxy`: Puppeteer `--proxy-server`; you can also set `HTTP_PROXY` / `HTTPS_PROXY`

### B1. Dual-Stub Template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

Outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate; `permissions` must cover every leaf code in the tree.

---

## C. Coverage Mode: Preload Configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before injection via CDP:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify: `window.__API_RECON_PRELOAD__ === true` and the pathname is stable.

Export the recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. Preload And Runtime Hook Capabilities

Built-in browser hook capabilities and coverage of preload (coverage) and runtime_harvest (depth):

| Hook capability | for API discovery value | coverage |
|---|---|---|
| Hook fetch / XHR.open | record requests URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Findings Authorization headers such as | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie read | identify session key names | ⚠️ optional `observe.storageReads/cookieReads` |
| Vue retrieve routes | fill in frontendRoutes | ✅ `__API_RECON_ROUTES__` (already-loaded routes) |
| Vue route-guard neutralization / Block login redirects | open up modules to trigger (APIs) API | ✅ `neutralizeVueRouter` + native redirect neutralization |
| React retrieve routes | supplement the routes | ⚠️ static + click; no dedicated Hook |
| page-navigation blocking (login path) | Stay-on-page analysis | ⚠️ Block only the login path path, avoid blocking business navigation |
| Hook Crypto library (CryptoJS/SM etc.) | encrypted params -> plaintext API body | ❌ requires manual (work) Hook Encryption-function input parameters; write conclusions into config config |
| Anti-debugging bypass | otherwise runtime runtime fails to record API | ❌ requires manual handling; static analysis still works |

---

## E. Endpoint Extraction Regexes (When Static Results Are Sparse)

Loosen `extract_endpoints` in `harvest_static.py`, or do it manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| symptom | cause -> handling |
|---|---|
| static API rarely | endpoint Dialect mismatch -> relax the regex (D section) |
| chunk count ≪ manifest | CSS-only or undeployed chunk; 404 retried |
| runtime Still shows the login page | Render-gate error -> recheck A1: key name, container, encoding, domain |
| shell loads but modules are blank | content gate -> forge menu (A3); `routes` path May be incorrect |
| Each route has only bootstrap/locale | Incomplete permission codes -> I section permission-tree restore; check `role_permissions` + `permissions/all` dual stub |
| Sidebar shows items but the subpage is blank | tree missing intermediate nodes or code and `userRouteAuth` mismatch |
| each API all skip login | interceptor gate -> confirm `neutralize`; nested fields need extended (walk logic) walk logic |
| WS frame count is (0) 0 | only after user interaction subscribe; Extend ... `perRouteMs` |
| empty response body | only `forward: true` Returns a real response only when enabled |
| Chromium missing | install chromium or set `config.chromium` / `CHROMIUM` |
| Mock many (mocks) still return to login | Hook hook installed too late, or missing `location.href` setter -> document-start + preload |
| List is completely empty | L3 empty arrays are fine; keep clicking Tab/settings/Details |
| mistaking (A for B) Redux action as routes | filter items containing get/set/change/clear/toggle/upload internal path |
| Vue Still bypasses login | preload non- document-start -> change the injection timing; or `neutralizeVueRouter: false` Manually clear the guards in that case |
| response contains (a URL) URL But missing from ... log | enable `extractUrlsFromResponse`; or from `__API_RECON_DETAIL__` Manual extraction |
| unknown Authorization header name | enable `observe.xhrHeaders` or DevTools Inspect request headers |
| runtime Extremely slow / timeout | edit `waitUntil: domcontentloaded`; lower `routeTimeout`; Do not use ... `networkidle2` |
| Proxy connection failure | check `proxy` / environment variable; Puppeteer and curl Keep proxy ports consistent |

---

## G. Hardened Targets

When the server validates the session step by step (unforgeable signed cookies, server-rendered menus that can't be stubbed), runtime gets stuck at the shell. Expected behavior:

- **Static analysis suffices for endpoint enumeration** -- module paths are in the code
- If authorized, run the same harness with a **real session**: `forward: true`, no neutralize needed, capturing real methods/params/responses

---

## H. Single-Run Task Checklist

1. Confirm the authorization scope
2. **Read** `scripts/harvest_static.py` -> adapt it to the target -> run it -> review `api_static.txt` and `routes.txt`
3. **Phase 1b**: path anchor-window expansion + binding layer -> `param_candidates.json` (Section J)
4. Reverse A1/A2/A3 -> write the site-specific `config.json`
5. **Read and adapt** `runtime_harvest.js` / `preload.js` before executing
6. `runtimeMode=depth`: `npm install` -> run the adapted harvest script
7. `runtimeMode=coverage/both`: inject the adapted preload at document-start -> dynamic enumeration via browser MCP + **parameter trigger matrix**
8. Module not rendering -> **permission-tree restoration per Section I** -> patch stubs -> re-run
9. Multi-sample parameter diff + error-based inference -> `params_merged.json`
10. Merge -> `site_map.json` + `api_merged.txt`, honestly marking coverage, gaps, and script modification points

---

## I. Permission Tree Reconstruction (Phase 4 Deep Dive)

Use when a simple forged `menus: [{ path, show: true }]` has no effect and sub-modules still don't mount.

### I1. Locating The Auth Module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: the **permission API path**, **response field names**, and **consuming chunk filenames**.

### I2. Extracting routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# Output: recon/route_map.json
```

If `[!] no routeMap pattern found`: loosen the regex in `extract_route_map.py`, or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Building The Permission Tree With Stubs

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases like `He=o.DASHBOARD`)
2. Resolve alias -> real path using `route_map.json`
3. Infer the parent from the code prefix (`MONITOR_ALERT` -> `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json`, and `role_permissions_stub.json`
5. With `--config`, automatically write `stubs` into `config.json` and extend `routes`

**Adapt to the target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` -> parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes without a prefix relationship

### I4. Verifying Stub Consistency

```bash
# permissions Count Should Match userRouteAuth Entries
wc -l recon/perm_codes_all.txt
# routes Should Cover Every Link In route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Re-Running Runtime And Comparing Results

```bash
node recon/runtime_harvest.js recon/config.json
# Comparing runtime_api.json Entry Counts Before And After forge And Checking For Module APIs Such As /attack And /asset
```

| forge before | forge after (Success) |
|---|---|
| Same across every route 3–5 entries bootstrap | Different routes trigger different module APIs module API |
| only `/api/locale/language` | appears `/api/web/...` module endpoint |
| `routes.txt` Single-digit route count | `routes` 80–110+ from route_map |

### I6. If It Still Fails

- **coverage mode**: click the sidebar + Tab -- permission gating may only issue requests after interaction
- **stub fields**: compare the nesting of the real API (curl + real session) against the stub
- **extra guards**: grep `hasPermission|checkRole|func.` button-level checks such as, extend `role_permissions.permissions`
- **Static fallback**: module API paths remain in `api_static.txt`; runtime only adds METHOD/body; keep parameters in `param_candidates.json` + recorded samples

---

## J. Parameter Reverse-Engineering (Phase 1b / 5b / 5c)

**A methodology, not a generic script.** Find paths with regex; find parameters via anchor-window expansion + UI binding chains + multi-sample diff + error-based inference.

### J1. Anchor Expansion — Finding Request-Building Objects From Known Paths

```bash
# Anchoring On Known Paths From Phase 1
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper Layer And Transport Shape

```bash
# axios And Unified Requests
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# Path Parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation Gate — Required Fields, Formats, And Enums

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding Layer — Forms To APIs

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Runtime fallback: DevTools -> Network -> request -> **Initiator** (call stack), tracing up from `fetch`/`send` to the packing function.

### J5. Encrypted Parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Never guess fields from ciphertext** -- hook the encryption function's **input parameters** and record the plaintext payload before encryption; write conclusions to `config.json` / `param_candidates.json`.

### J6. Parameter Trigger Matrix (Required In Phase 3)

Record once per operation for each module, then diff the request body/query:

| Actions | actions/follow |
|---|---|
| First screen of the list | default page size |
| search | keyword, filters |
| advanced filters | optional field |
| New/Edit | complete entity |
| bulk/export | `ids[]`, `exportType` |
| sorting/paginate | `sortField`, `order` |

Produces `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence Rules

| confidence | condition |
|---|---|
| **high** | static callsite + runtime ≥2 Samples agree |
| **Medium** | Static-only, or only 1 time(s) runtime |
| **Low** | response/reverse-infer from errors, Not verified a second time |
| **pending trigger** | statically known fields, UI/Permissions not yet reached |

### J8. Quick Scenario Presets

| scenario | order |
|---|---|
| REST list page | J1 request-building object -> J6 four rounds diff -> J3 rules |
| New/edit form | J3 Form name -> J4 submit chain -> runtime submit + deliberately leave empty to see (the 400) 400 |
| GraphQL | J2 variables declaration -> runtime each operation record variables |
| encrypt body | J5 Hook input params -> Pre-encryption fields are the real params params |

### J9. Mapping To api-recon Phases

| api-recon | parameter recon |
|---|---|
| Phase 1 static | J1 anchor window expansion |
| Phase 2 A2 interceptor | globally injected fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | Different modules have different forms -> Full fields trigger only with sufficient permissions |
| Phase 5 merge | `params_merged.json` + confidence; don't decide required/optional from a single sample |

### J10. Troubleshooting

| symptom | handling |
|---|---|
| field names found statically runtime Never appears | mark"pending trigger"; complete the permission tree / open advanced filters / linked cascading select each option |
| same path different body shape | normal -- by `action` Record as separate entries, Do not force-merge schema |
| stub response is stubbed but you want to inspect params params | **inspect outbound request** body/headers, Do not derive from ... stub reverse-inferring from the response |
| 400 report nested field | Watch for outer wrappers `data`/`bizData`/`variables` |
| GraphQL Only the ... is visible operation name | expand `variables` JSON; locate statically `$var: Type` |

---
