# Upstream response audit hardening — changes (spec A–I)

Branch `feat/audit-hardening`, base `d958c3af2`. Spec: `../audit-hardening-spec.md`.
Section J (watchdog) is handled in the channel-watchdog repo and is not part of this branch.

Legend: **done** = implemented as specified; **done (deviation)** = implemented with a
documented difference; **not done** = with reason.

## A. Verdict logic — `service/upstream_audit.go`

| Item | Status | How |
|---|---|---|
| A1 | done | `snapshotAliasVerdict` + `isSnapshotDateSuffix`: only `YYYY-MM-DD`, `YYYYMMDD`, `YYMMDD`, `MMDD` and Google's `MM-DD` / `MM-YYYY` (e.g. `gemini-2.5-flash-preview-05-20`, `-09-2025`) are dates, with month 01–12 / day 01–31 checked. Alias→dated = `snapshot_date`; dated→alias = new verdict `pinned_to_alias` — **not a mismatch** (owner decision 2026-09-28: official DeepSeek answers `deepseek-v4-pro-0813` as `deepseek-v4-pro`, 107/7d on #135); recorded in `ursp.kind`, shown in the panel without highlighting, evidence kind stays `normal` with note `pinned_to_alias`. `VerdictIsMismatch` (only `mismatch`) is the single decision point. `deepseek-v3`→`deepseek-v3-1-250821` and `gpt-5`→`gpt-5-1234` are mismatches. |
| A2 | done | `latestFamilyMatch` rejects a response carrying a lower-tier token the alias does not name, except `preview` for the gemini family only (Google ships the build a `-latest` alias points at as `…-preview`; `gemini-flash-latest` → `gemini-2.5-flash-preview-09-2025` is a rolling alias, `…-flash-lite-preview-…` is not). `preview` remains a tier token for the downgrade label. Version-older check for `-latest` **not added** (spec: only if cheap; a correct "older than the alias" needs a vendor release table). |
| A3 | done (deviation) | `canonicalModelPair`: region + vendor segment stripped; `-vN[:M]` compared when both sides carry it, stripped from the side that alone carries it — including the sender (spec only says "strip from response"; needed because the Bedrock body reports the vendor name while the wire carries the Bedrock id, see B2). `-vN` on non-Bedrock names (`deepseek-v3`) is never stripped. Vertex `@YYYYMMDD` pins (and `-vN@YYYYMMDD`) are normalised to `-YYYYMMDD`, so `claude-sonnet-4-5@20250929` / `claude-3-5-sonnet-v2@20241022` on the wire match the Claude body's `claude-sonnet-4-5-20250929` / `claude-3-5-sonnet-20241022`. |
| A4 | done | `splitVendorPrefix` returns the prefix; recorded as `ursp.prefix` and appended to the backend-tracker signature (`gpt_relay/deepseek`). |
| A5 | done | `-NNN` suffix on `gemini-*`, alias→versioned only: `version_suffix`; versioned→alias = `pinned_to_alias`. |
| A6 | done | `UpstreamModelIsTierDowngrade` rewritten: replacement forms (pro→flash, pro→flash-lite, flash→flash-lite, opus→sonnet/haiku, sonnet→haiku, 4o→4o-mini, max→plus), responses without the family word (#57), `turbo` removed from lower tiers. New `UpstreamModelIsVersionDowngrade` → `ursp.version_downgrade`. Both gated on mismatch. |
| tests | done | `service/upstream_audit_test.go`: `TestVerdictHardening`, `TestCanonicalModelPairRecordsPrefix`, `TestTierAndVersionDowngradeReplacementForms` (positive + negative, incl. reviewer counter-examples); existing tables updated where the stricter rules intentionally flip a case. |

## B. Sent model = wire model

| Item | Status | How |
|---|---|---|
| B1 | done | `relay/common/relay_info_audit.go` `SnapshotUpstreamRequest` (called from `relay/channel/api_request.go` `doRequest`): body `model` via `req.GetBody` — bodies of known size ≤1 MB are read in full (model + prompt hash), anything larger or of unknown size only through a 64 KB prefix (model only, no hash), so disk-backed bodies are never pulled back into memory — else Gemini/Vertex/Bedrock URL model segment, else `UpstreamModelName`. `ursp.sent` = wire, `ursp.sent_field` = `UpstreamModelName` when different. Also computes the prompt hash and explicit temperature (E7/H4). |
| B2 | done | `relay/channel/aws/relay-aws.go`: `SnapshotUpstreamWire(awsModelId, body)` for Nova, stream and non-stream InvokeModel. Test `TestDoAwsClientRequestSnapshotsBedrockModelId`. |
| B3 | done | Removed `TestSnapshotHappensAfterRequestSideRewrites`, `TestSnapshotMakesHonestEchoCompareClean` (relay/common) and `TestHonestEchoOfStrippedModelComparesClean` (relay/helper). New `relay/channel/api_request_sent_model_test.go` drives `doRequest` against an httptest upstream: converted body, pass-through + mapping, `param_override` model rewrite, Gemini URL model, prompt hash/temperature, per-attempt reset. |

## C. Observation coverage

| Item | Status | How |
|---|---|---|
| C1 | done | `ursp.obs` (`none`/`partial`/`full`, `UpstreamObservationLevel`) and `ursp.h` (handler name via `MarkUpstreamObserver`, relay-mode fallback) on every text log and on the error path, even when nothing was observed (no evidence row then). Task and Midjourney logs get `{obs:none,h:task|mj}`. |
| C2 | done | Instrumented: xAI chat (stream/non-stream), AWS claude/nova, Responses WebSocket (`relay/responses_websocket.go`), ollama, cohere, zhipu legacy, baidu, tencent, xunfei, coze, dify, palm, cloudflare chat — model/id/created where the protocol has them, headers + excerpt otherwise. Image responses (OpenAI, Gemini): `ObserveUpstreamMediaResponse` keeps headers + first 4 KB with obs=none. Audio/rerank/embedding/realtime get obs=none with a handler name derived from the relay mode. Xunfei's excerpt is the re-encoded websocket frame (the SDK hands us structs, not bytes). |
| C3 | done | `service/upstream_audit_error.go` `RelayErrorHandlerWithAudit` wraps `RelayErrorHandler` (same error returned) and is used by every relay handler including `claude_handler.go`; error logs get `ursp.err` + evidence. Trackers are not fed from error bodies. |
| C4 | done | `chat_via_responses.go` / `responses_via_chat.go` store excerpt + headers, model frames and output hashes. |

## D. Streaming

| Item | Status | How |
|---|---|---|
| D1 | done (deviation) | `ObserveUpstreamResponse` keeps first model + distinct set (`ursp.models`). `mid_stream_change` flag and permanent evidence kind `mid_stream_change`. A change = any two declarations in the stream that are not benign renames of each other (pairwise; alias→dated snapshot or vendor prefix alone is not a change) — see "Deviations". The model sequence always goes into the evidence note. |
| D2 | done | New columns `model_frame` (first model-bearing frame, 16 KB, UTF-8 safe; for non-stream bodies over 64 KB the body tail) and `error_frame`. Claude stream error frames no longer overwrite the `message_start` excerpt. OpenAI usage-only final frames keep the model frame (test `TestOaiStreamHandlerKeepsModelFrameBehindUsageOnlyFrame`). |

## E. Evidence integrity

| Item | Status | How |
|---|---|---|
| E1 | done | `common/audit_text.go` (`TruncateAuditBytes`, `TruncateAuditBytesTail`, `TruncateAuditRunes`, `SanitizeAuditText`): rune-boundary cut, NUL stripped, invalid UTF-8 replaced. Columns `excerpt_truncated`, `excerpt_full_len`. |
| E2 | done | `UpstreamEvidence.sanitize()` truncates every varchar by characters before insert. |
| E3 | done | Raw bytes captured before the OpenRouter enterprise unwrap and before the SGLang `created_at` rewrite (non-stream and stream); comments fixed. Tests in `relay/channel/openai/upstream_audit_observe_test.go`. |
| E4 | done | `EncodeUpstreamAuditHeaders`: all values per key (joined `", "`, keeps the `map[string]string` JSON shape readers already parse), credentials `[redacted]`, 8 KB cap keeps what fits plus `"_headers_truncated":"true"`. |
| E5 | done | `model/upstream_evidence.go`: the writer is started explicitly after `InitLogDB` (`model.StartUpstreamEvidenceWriter`, so health is accurate right after a restart); one writer on a 4096-row queue with a 64 MB byte budget (rows beyond either limit are dropped + counted), batches of 200 or 1 s, failed batch retried row by row. `last_error` has DSNs, host=/user=/password= parameters and host:port pairs redacted. Counters + `GET /api/audit/health` (`middleware.AdminAuth` + `RequirePermission(authz.AuditRead)`, same as `/api/audit`, `controller/upstream_audit.go`, `router/api-router.go`) returning `{"success":true,"data":{inserted_total,dropped_total,failed_total,last_error,last_error_at,queue_len,queue_cap,started_at,enabled,…}}` (+ `disabled_reason`, `index_state`). `main.go` flushes (≤10 s) after `srv.Shutdown`. |
| E6 | done | Writer start: `LOG_DB == nil` or a ClickHouse log DB disables persistence with one warning (`enabled=false`, no per-request errors). A missing table only pauses it (`disabled_reason` "…retrying"); the check is retried with backoff (30 s doubling to 10 min) and persistence switches on when the table appears. |
| E7 | done (deviation) | Columns `retry_index`, `prompt_sha256`, `output_sha256` (added by AutoMigrate, nullable, no default). Prompt hash: sorted-key canonical form of messages/input/prompt/contents + system/instructions/systemInstruction + tools of the body actually sent. Output hash: whitespace-normalised natural-language output (content + reasoning; tool calls excluded so agents repeating an identical tool call are not replays). Prompt hash only for sent bodies ≤1 MB, computed with gjson sort-keys/ugly modifiers on raw bytes. Indexes `(channel_id, response_id)` and `(channel_id, output_sha256)` are **not** built automatically by default: they are created manually (SQL below). With env `AUDIT_EVIDENCE_AUTO_INDEX=true` a background job builds them 2 min after the writer starts (default off: health `index_state=disabled`): PostgreSQL `CREATE INDEX CONCURRENTLY IF NOT EXISTS` on one pinned session holding `pg_try_advisory_lock` (other instances skip); an INVALID leftover is dropped concurrently and rebuilt only when `pg_stat_progress_create_index` shows no build of it in progress (on PostgreSQL < 12, where that view is missing, it is left alone and `index_state` reports the error), MySQL online `ALGORITHM=INPLACE LOCK=NONE` after `HasIndex`, SQLite `CREATE INDEX IF NOT EXISTS`. No GORM index tags. Deviation: hash columns are `varchar(64)`, not `char(64)` — see "Deviations". |
| E8 | done | Pruner deletes expired `normal` rows in 5000-row batches ordered by `created_at, id` (served by the existing `created_at` index), deleting by primary key, 200 ms pause, progress log every 20 batches, recover per run inside the loop. |
| E9 | done | `SetUpstreamResponseForensicsBytes` slices before converting; all `string(responseBody)` forensics call sites replaced. |
| E10 | done | `setting/audit_setting`: option key `audit_setting.normal_excerpt_mode`, default **`full`**; `digest` keeps first/last 2 KB of excerpt and model frame on `normal` rows only (hashes always kept, anomalies always full). |
| E11 | done | Stale "only persisted on mismatch" comments rewritten (`relay_info.go` fields, forensics helpers, OpenAI stream handler). |

## F. Trackers / state

| Item | Status | How |
|---|---|---|
| F1 | done | Source/backend trackers keyed by `flipTrackerKey{channelId, family, keyIndex}`. The backend signature adds only the vendor segment of the response prefix (`openrouter/deepseek`→`deepseek`, `us.anthropic`/`global.anthropic`→`anthropic`, Gemini `models/` ignored); a flip is a change of shape, or of vendor between two answers that both carry one — prefix present↔absent alternation is not a flip, and the last vendor is remembered across unprefixed answers. |
| F2 | done | `RelayInfo.IsChannelTest` (already set by `controller/channel-test.go`): no tracker/ccmax/replay-memory writes (lookups only), evidence note `channel_test`. No change in `channel-test.go` was needed. |
| F3 | done | `ccmaxObserve`: sticky only after 2 visible injections within 1 h, expires 24 h after the last one; a visible injection still labels its own request. |

## G. Public log leak

| Item | Status | How |
|---|---|---|
| G1 | done | `upstream_model_name` written to `admin_info` (text logs `service/log_info_generate.go`, task logs `service/task_billing.go`); `model/log_other.go` adds it to the legacy-sensitive keys, so it can no longer be set publicly and older rows lose the top-level copy in the user projection (admins keep it). Frontend reads `admin_info` first with the legacy fallback. Contract tests: `TestTextLogKeepsUpstreamModelOutOfPublicScope`, `TestUserProjectionHasNoUpstreamDeclaredModel`. Visible change: log owners no longer see the mapped model name. |

## H. Replay / authenticity signals — `service/upstream_audit_signals.go`

| Item | Status | How |
|---|---|---|
| H1 | done | `stale_created` outside `[start−120 s, now+120 s]`, ms stamps normalised. |
| H2 | done | `dup_id` via a 100k-entry LRU keyed by channel + id; response ids are capped at 255 chars when observed (bounds the LRU memory, same as the DB column); ignores empty, <8 chars, prefix-only/all-zero ids and repeats by the same request id (retries). An output-less turn never overwrites the output hash remembered for an id. |
| H3 | done (deviation) | `dup_output` via the same LRU keyed by channel + output hash; the cross-id duplicate ignores outputs < **200** normalised text chars (spec: 64 — raised after false positives: agent openers and short JSON repeat honestly). The output hash excludes tool calls. |
| H4 | done (deviation) | `replay` requires corroborated evidence of the same earlier answer: (dup_id AND both requests have the same non-empty text output hash AND at least one of: stale_created that corroborates on this channel (see below), the current `created` is stale AND equals the `created` remembered for that id (holds even on a constant-created channel), or text ≥200 normalised chars for a different prompt (both prompt hashes present and equal = an honest repeated request, never corroborates) — so the #148 cache (same id, same 22-day-old created, same short greeting) is caught while a constant-id upstream answering "OK" with fresh stamps, two fresh answers sharing a one-second stamp, or an identical prompt answered deterministically are only `dup_id`) OR (dup_output across different ids, ≥200 chars, AND a corroborating stale_created, unless the earlier answer already carried this very stale created — a cache hands back an answer that was fresh when first produced, while a #109-shaped channel stamps every answer with the same old value; this also covers the cold start before the constant-created tracker has seen two different answers, review round 5). The former "dup_id on that same earlier row" branch was removed in review round 4: it is the same-id case and bypassed the same-prompt guard. The constant-created tracker is fed only stale answers (fresh answers on a busy channel share one-second stamps). A cache stamping many different answers with one old created marks its channel constant-created, which disables the cross-id dup_output+stale rule there; the same-id path above still catches it. stale_created stops corroborating on a channel detected to stamp a constant created (#109: the same stamp seen with two different ids and outputs; one small entry per channel, reset when the stamp changes) and stays a recorded flag there. dup_id alone, dup_output alone, an empty output hash (tool-call-only turns, handlers without a hash) never make a replay; every non-empty text output is hashed and remembered, and an output-less turn never overwrites the remembered hash/created. `prompt_differs` and explicit temperature > 0 are recorded reasons only. Evidence kind `replay_suspect` (permanent). Notes: `provider_switch` rows keep exactly the flip text (`azure -> newapi_relay`, the watchdog groups on it); every other kind starts with `signals: …` so the 255-char cut never drops the reasons, followed by replay_suspect / pinned_to_alias / error / channel_test / provider-also-flipped / model sequence. |
| H5 | done (deviation) | `implausible_speed`: completion ≥ 200 and > 400 tok/s. Streams use use_time − frt as specified; non-stream responses have no first-token time, so the whole duration is used — see "Deviations". |
| H6 | done | `extra_keys`: sorted top-level keys outside the OpenAI chat / Responses / Claude / Gemini standard sets (Claude/Responses stream events are unwrapped to the response object). Not computed for failed attempts. |
| H7 | done | `pt_ratio` = settled input tokens (incl. Claude cache read/creation) / local estimate, 2 decimals, skipped for locally counted usage. Settled counts come from a one-line observation hook in `service/text_quota.go`. |
| tests | done | Happy path carries no flag; replay matrix; channel tests; speed/keys/ratio (service tests). |

## I. Frontend — `web/src/features/usage-logs`

done. `types.ts` (new verdicts/fields, `admin_info.upstream_model_name`), `lib/format.ts` (`isUpstreamAuditAnomaly`, `mappedUpstreamModelName`), details dialog rows for obs/handler, sent_field, prefix, declared model sequence, pinned_to_alias/version_suffix, version_downgrade, mid_stream_change, replay, stale_created/dup_id/dup_output/implausible_speed, extra_keys, pt_ratio, retry; table rows and mobile cards highlight replay / mid_stream_change like mismatch. `zh.json` (22 keys under `translation`; duplicate-key check with `object_pairs_hook` passes, top level only `translation`). `en.json` is not updated: it does not carry the fork's audit keys (English keys are their own fallback). Tests: `lib/__tests__/upstream-audit.test.ts`, `components/__tests__/upstream-audit.test.tsx`.

## Deviations

1. **E7 `char(64)` → `varchar(64)`.** GORM's PostgreSQL migrator reports `char` columns as `bpchar`; a type mismatch on the next start could make AutoMigrate issue `ALTER COLUMN … TYPE` on the 37 GB table. `varchar(n)` is the type the existing columns already use without churn. Values are always 64 hex chars.
2. **H5 non-stream timing.** `use_time − frt` is ~0 for a non-stream response (first byte = whole body), which would flag every long non-stream answer. Non-stream uses the whole duration.
3. **D1 benign renames.** See the D1 row.
4. **Health payload** carries extra fields (`disabled_reason`, `index_state`, `queue_bytes`).
5. **A3** strips a lone `-vN[:M]` from the sent side too (see A3 row).
6. **H3/H4** replay needs corroboration from the same earlier answer; minimum output 200 chars (see H3/H4 rows).
7. **`preview` in gemini `-latest` resolution** is not treated as a lower tier (see A2 row).
9. **pinned_to_alias is not a mismatch** (owner decision; the spec A1 said it should count).
10. **E7 automatic index build is opt-in** (`AUDIT_EVIDENCE_AUTO_INDEX`, default off); the spec asked for a background build.
8. **i18n**: only `zh.json` is updated. The fork's existing audit keys are only in `zh.json` (en/zh-TW/ja/fr/ru/vi do not carry them; English keys fall back to themselves), so the new keys follow that convention.

## Deployment notes and risks

- **Schema — run before deploying** (AutoMigrate would otherwise add the 7 nullable, default-less columns at startup; metadata-only on PostgreSQL, but the `ACCESS EXCLUSIVE` lock can queue behind a long query on the table and stall startup; production has `lock_timeout=0`). Retry until it succeeds; each attempt gives up after 3 s instead of blocking traffic:

  ```sql
  SET lock_timeout = '3s';
  ALTER TABLE upstream_evidences
    ADD COLUMN IF NOT EXISTS model_frame text,
    ADD COLUMN IF NOT EXISTS error_frame text,
    ADD COLUMN IF NOT EXISTS excerpt_truncated boolean,
    ADD COLUMN IF NOT EXISTS excerpt_full_len bigint,
    ADD COLUMN IF NOT EXISTS retry_index bigint,
    ADD COLUMN IF NOT EXISTS prompt_sha256 varchar(64),
    ADD COLUMN IF NOT EXISTS output_sha256 varchar(64);
  ```
  (`bigint` is what GORM uses for Go `int` on PostgreSQL, so AutoMigrate finds nothing to change afterwards.) MySQL before 8.0.12 (no INSTANT ADD COLUMN) would rebuild the table — production is PostgreSQL. The table is in both the main and log AutoMigrate lists (unchanged); with a single database the second pass is a no-op.
- **Indexes — run manually off-peak** (not built automatically unless `AUDIT_EVIDENCE_AUTO_INDEX=true`). `CONCURRENTLY` cannot run inside a transaction; run each statement on its own (psql autocommit). Production has `statement_timeout=0`, so the builds are not cancelled by the server:

  ```sql
  CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_upstream_evidences_channel_response_id
    ON upstream_evidences (channel_id, response_id);
  CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_upstream_evidences_channel_output_sha256
    ON upstream_evidences (channel_id, output_sha256);
  ```
  Check progress / validity, and clean up an interrupted (INVALID) build before re-running the CREATE:

  ```sql
  SELECT c.relname, p.phase, p.blocks_done, p.blocks_total
    FROM pg_stat_progress_create_index p JOIN pg_class c ON c.oid = p.index_relid;
  SELECT c.relname, i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
   WHERE c.relname LIKE 'idx_upstream_evidences_channel_%';
  -- only if indisvalid = false and no row above shows a build in progress:
  DROP INDEX CONCURRENTLY IF EXISTS idx_upstream_evidences_channel_response_id;
  DROP INDEX CONCURRENTLY IF EXISTS idx_upstream_evidences_channel_output_sha256;
  ```
  Nothing in the gateway needs these indexes to function; they serve replay look-ups (watchdog / manual queries).
- **Behaviour on live traffic** (evidence counts will shift; not measurable locally):
  - new `model_mismatch` rows: suffixes no longer accepted as dates (`-2507`-style YYMM, `-1234`, extra version segments), `-latest` resolving to a lower tier (gemini `-latest` → `…-preview` stays accepted). `pinned_to_alias` (e.g. official DeepSeek `deepseek-v4-pro-0813` → `deepseek-v4-pro`, 107/7d; `deepseek-v4-flash-0731` → `deepseek-v4-flash`, 28/7d) is recorded as a normal row with that note, not a mismatch;
  - B1: pass-through channels with model mapping are now judged on the model actually in the body; AWS/Vertex logs carry `sent_field` because the wire id differs from the channel name;
  - error logs (~3.9k/day) now get ursp and, when a body was observed, evidence rows; image responses get 4 KB rows; legacy channels (cohere, baidu, …) get rows with obs=partial;
  - `provider_switch` rows drop for multi-model / multi-key channels; a relay answering with a different vendor namespace than before (both prefixed) now flips;
  - `replay_suspect` / `mid_stream_change` are new permanent kinds, both requiring corroboration (see D1/H4);
  - log owners no longer see the mapped model name (G1).
- **Per-request cost**: the send boundary reads ≤1 MB (or a 64 KB prefix) of the outgoing body; replay memory ≤100k entries (≈15–20 MB).
- **Shutdown** waits up to 10 s more to flush queued evidence.
- **Health polling**: `/api/audit/health` is behind `AdminAuth` + `AuditRead`; the watchdog's root access token passes (verified by the owner: role 100). Each poll with a personal access token writes one PAT access-audit row, i.e. ~1440 rows/day at a 60 s interval.

## Verification

All run locally on the final commit (Go 1.27.1, bun):
- `go build ./...` — ok; `go vet` on all touched packages — ok; `gofmt -l` on every changed Go file — clean.
- `go test -count=1` service, relay/common, relay/helper, relay/channel (+ openai, claude, gemini, xai, aws, ollama, tencent), model, relay, controller, router, common — all ok.
- Audit tests `-count=3 -shuffle=on` (service, relay/common, relay/helper, relay/channel, openai, claude, aws, model, controller) — ok.
- `go test -race`: relay/common, relay/channel/openai, relay/channel/claude ok; service ok with `TestUpdateVideoTasks*` skipped; model ok with `TestReserve*|TestSynchronousReserve*|TestBatchUpdate*` skipped (the audit tests there are also race-clean at `-count=3`). The skipped tests race in files this branch does not touch — `service/task_polling.go`/`logger`, and the async `DecreaseUserQuota` goroutines vs. `useUserCacheMiniRedis` in the quota/user-cache tests. Both races also happen on the untouched base commit `d958c3af2`, but intermittently: the whole model package failed `-race` on the base in 3 of 3 local runs, and the independent reviewer reproduced it in 1 of 2.
- web: `bun install`, `bun run typecheck` ok; `vitest run src/features/usage-logs` 23 files / 413 tests ok; oxlint on touched files clean; zh.json duplicate-key check (object_pairs_hook) ok, top level only `translation`.
Not verified: real MySQL and PostgreSQL instances (no Docker / database servers on this machine), so the AutoMigrate and index paths were exercised on SQLite only.
