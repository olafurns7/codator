# Provider probe contract

Codator remains the account-selection CLI for agents and Herdr launchers. Native arguments, existing flags, JSON status fields/schema version, and exit codes are unchanged. `status --refresh` is additive and combines with `--json` and the optional provider in any order.

## Failure policy

| Observation | Claude retry policy | Launch behavior |
| --- | --- | --- |
| Complete usable response, including a known exhausted window | Cache for 5 minutes | Automatic ranking uses current eligible headroom; explicit selection cannot bypass exhaustion |
| Transport, DNS, connection failure, or timeout | Retry after 60 seconds | Automatic selection skips it; explicit selection may use a previously verified subscription with unknown quota |
| Authentication rejection (401/403, native auth code, or recognized login error) | Remember until login or explicit refresh | Both selection paths refuse it and show `codator login claude NAME` or `codator login codex NAME` |
| Provider rate-limit rejection (429 or equivalent) | Retry-After, or 15 minutes when absent/invalid | Unknown quota can launch explicitly only with prior subscription verification and no retained denial |
| Malformed response or unrecognized native error | Retry after 60 seconds | No automatic selection; a malformed response cannot grant explicit permission |
| Partial valid response with unknown quota | Retry after 60 seconds | Existing model-specific valid results remain usable; verified identity with unknown quota retains existing explicit behavior |
| Interrupted/unresolved probe | Reservation expires after 60 seconds | Fail closed for explicit launch until a completed result; an auth refresh retains its auth denial |

The 60-second transient retry lets an outage recover promptly while limiting repeated native processes and requests. Five-minute successful caching is unchanged. Native probes retain their existing 20-second timeout. Native error bodies are classified in memory. Exact status/code/type fields in the native error and its error/data envelopes take precedence; unrelated nested metadata is ignored. Text fallbacks accept recognized native HTTP-error prefixes or complete auth messages, never incidental numbers or embedded denial phrases. Retry-After is read only from the error envelope or its headers. Only fixed cause/fix text, failure class, and safe retry time are returned or stored; raw error messages and provider bodies are never included in diagnostics or caches.

`Retry-After` accepts nonnegative seconds or an HTTP date. Values beyond one year and past HTTP dates are ignored before storing a deadline; an unusable value retains the 15-minute fallback. Cache validation also refuses rate-limit deadlines beyond 366 days. Quota reset times remain separate from probe retry deadlines.

## Explicit launch and denials

Subscription identity and available quota are separate facts. A completed Claude transport or rate-limit failure can retain subscription identity from a previously sanitized native response even after the old usage observation expires. It grants no headroom and never makes the account automatically eligible. A fresh valid non-subscription response revokes that identity.

Known exhausted model/shared windows survive failed and partial refreshes until their reset time; model scope remains unchanged. A still-valid exhausted snapshot also remains a denial. Auth rejection overrides cached identity. A forced auth refresh that fails on the network or with malformed data keeps the auth denial. A completed usable Claude response or a validated known Codex quota result (eligible or exhausted) can recover it; `codator login PROVIDER NAME` clears the old observation/denial before running native login, allowing an immediate post-login probe.

After a failed Claude refresh, status hides the prior snapshot percentages even while that snapshot is young. It retains observed/retry timestamps and projects known exhausted windows separately, with the existing JSON fields/schema unchanged.

Unknown native errors are classified as malformed rather than assumed to be a network outage. Valid partial quota payloads still use the existing per-model parser and denial projection.

## Refresh, locking, and migration

`codator status --refresh` bypasses Claude's normal success/failure cooldown, including a stored auth rejection, but never the account lock. It permits at most one attempt per account every 10 seconds, measured from the last attempt (including ordinary probes). This permits quick owner recovery while coalescing repeated refresh commands. The durable reservation is written before starting the native process. Busy profiles are still reported busy; force does not take their locks away.

Claude cache version 3 retains the bounded, private, sanitized snapshot and denial format. Version 2 caches are fully validated before migration: valid successful observations keep their five-minute window; old failed/unknown/reserved attempts use the original attempt time plus 60 seconds. Denials remain intact; old failed snapshots and identity/allowance claims are discarded because those failures could have been expired logins. An ambiguous legacy failed reservation does not become transport proof or launch permission. Invalid caches receive a short fail-closed reservation; unsafe permissions, symlinks, or write failures remain errors. A failed result write cannot report a new allowance that was not durably recorded.

Codex has no persistent quota cache or 15-minute lockout. Its native account/read check verifies subscription identity on each probe, and a transport failure during the following usage check can retain that current identity for explicit launch. Codex persists only a private auth marker and the last auth-refresh attempt, using the same bounded file/atomic-write checks as Claude. Ordinary calls do not retry a stored auth rejection. Explicit status refresh retries it after a 10-second minimum; transport, malformed, empty or partial/unknown quota results do not clear it. A known exhausted result may clear auth while remaining quota-ineligible; known eligible quota may recover normal launch. Live non-auth Codex probes retain their existing behavior without a new usage cooldown.

## Validation boundary

Tests use native CLI protocol fakes only. They verify failure classes, safe diagnostics, retry windows, explicit launch, auth persistence/recovery, forced refresh, legacy migration, and concurrency. They do not prove current provider protocol compatibility, real login renewal, macOS credential-store behavior, or deployment. Those require separate owner-authorized checks; this implementation does not read live credentials or probe providers.
