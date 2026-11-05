# Internal HTTP contract

All paths are same-origin. JSON responses are direct objects/arrays; failures are `{error:"..."}` with HTTP status. POST/PUT/DELETE require a matching Origin or `X-Ops-Request: 1` with no conflicting Origin. Every fetch should send `X-Ops-Request: 1`. Cookies are HttpOnly, browser credentials same-origin. Do not store credentials in localStorage. Render all generated/user text with textContent, not innerHTML.

## Authentication and setup
- `GET /api/status` public: `{initialized,version}` only.
- `POST /api/setup/unlock` `{code}` -> `{ok:true}` and setup cookie.
- `GET /api/setup/config` setup cookie: `{config, environment, presets}`.
- `POST /api/setup/complete` `{config, secrets:{model_key,official_secret,onebot_token}}` -> `{token}` (show once, require user copy; then login). Uses same config schema as `internal/config/config.go`. Channels can be configured but enabling live mode during setup is prohibited; enable later via settings.
- `POST /api/login` `{token}` -> `{ok:true}` and login cookie.
- `POST /api/logout` -> `{ok:true}`.
- `GET /api/config` authenticated: `{config,secrets_configured:{model_key,official_secret,onebot_token},presets,environment}`. No raw secrets.
- `PUT /api/config` `{config,secrets:{...},confirm_token}`. Empty/omitted secret values preserve existing secrets. Used to update persona/watch/channels/etc. Access token reconfirmation required; handle via modal. Never echo keys back.
- `POST /api/test/model` `{model:{base_url,name},api_key}` accepted in setup/login session. If API key blank, use saved key. Explicit user action only, confirmation describing outbound fixed test message required.
- `POST /api/token/rotate` `{confirm_token}` -> `{token}`, invalidates sessions.
- `GET /api/metrics` -> platform-specific metrics object; unsupported Linux counters clearly marked on Windows.

## Conversations and tasks
- `GET /api/sessions` -> array of `{id,title,scope,channel,created_at,updated_at}`.
- `POST /api/sessions` `{title}` -> session.
- `GET /api/sessions/{id}` -> `{session,messages,tasks}`; messages `{id,session_id,role,content,created_at}`. Tasks include id/session_id/input/status/output/error/created_at/updated_at/dry_run.
- `DELETE /api/sessions/{id}` deletes history after confirmation; rejects active tasks.
- `POST /api/tasks` `{session_id,message}` -> task. Missing session_id creates web session.
- `GET /api/tasks` -> recent tasks.
- `POST /api/tasks/{id}/cancel` -> `{ok:true}`.
- `GET /api/approvals` -> pending `{id,task_id,kind,name,preview,created_at}`; exact call held by backend.
- `POST /api/approvals/{id}` `{approve:true|false}` -> `{ok:true}`; never edit arguments in approval.
- `GET /api/events?after=ID` SSE. `event: update`, JSON `{id,kind,task_id,session_id,data,created_at}`. Updates include task, text, tool, approval, memory, channel, watch. On any update refresh targeted data or debounce refresh selected conversation/timeline/approvals. EventSource native reconnect; Last-Event-ID supported. Bound batches. No secrets or hidden model reasoning.
- `GET /api/timeline` -> recent event array.
- `GET /api/brief` -> `{text}` deterministic summary, not fabricated model findings.

## Memory and persona
- `GET /api/memories` -> items array `{id,text,category,subject,scope,source,status,importance,created_at,updated_at,expires_at,superseded_by}`. Status active, archived, superseded.
- `POST /api/memories` -> upsert item (explicit user save confirmation; id only for existing). Default scope owner, source user, category fact. Use item fields from `internal/memory` JSON tags when available.
- `DELETE /api/memories/{id}` -> `{ok:true}`.
- `POST /api/memories/{id}/state` `{status,superseded_by}` -> `{ok:true}`.
- `GET /api/memories/search?q=...` -> hits `{item,score,why}`. Empty query returns no hits.
- Persona presets returned with config; edits go through PUT config.
- Memory citations are event kind `memory`, data `{hits:[...]}`; show readable source/updated_at, do not surface unrelated scopes to group channels.

## Channels / watch
- `GET /api/channels` -> `{official:{status,enabled,dry_run},onebot:{...}}`.
- `POST /api/channels/{channel}/test` explicit connectivity check using saved settings; does not send a business message. channel official or onebot.
- `GET /api/bindings` -> `{bindings:[{id,channel,bot_id,user_id,created_at}],pending:[{id,channel,code,user_id,expires_at}]}`.
- `POST /api/bindings` `{channel}` -> pending pair. User sends `/bind CODE` to bot; web shows candidate user then approves.
- `POST /api/bindings/{id}/approve` -> `{ok:true}`.
- `DELETE /api/bindings/{id}` -> `{ok:true}`.
- `GET /api/notifications` -> notification array; fields id/metric/state/message/value/threshold/created_at.
- Watch settings reside in config.watch; disabled by default. Channel dry_run blocks outgoing messages, preview in timeline. Enabling a channel while saving settings intentionally connects externally; warn before save.

Prototype label: **NEXUS / OPS**, Chinese UI; product identity distinct from Aurabot. No stock photos, desktop pet, remote fonts or copied assets.
