# Fork additions

**qx-whatsapp-mcp** is a fork of [`verygoodplugins/whatsapp-mcp`](https://github.com/verygoodplugins/whatsapp-mcp)
that adds WhatsApp **group management**, **communities**, **profile pictures** and
**slow-send jobs** (one personal message to each of many people, paced like a human).
Everything lives in new files so the diff against upstream stays small:

| Component | File | Upstream touch |
|---|---|---|
| Bridge endpoints | `whatsapp-bridge/groups.go`, `communities.go`, `group_picture.go` (+ `_test.go`, `testdata/`) | one `registerGroupRoutes(...)` line in `main.go` (it registers the community and picture routes too, and passes the media roots) |
| Slow-send jobs | `whatsapp-bridge/send_jobs.go` (+ `send_jobs_test.go`) | one `registerSendJobRoutes(...)` line in `newRESTMux`, plus starting the runner before the REST server and stopping it before `client.Disconnect()` in `main()` |
| Bridge dependency | `golang.org/x/image` (Go team) | WebP decoding and image scaling for group pictures; v0.45.0 is the newest that builds with Go 1.25 |
| MCP tools | `whatsapp-mcp-server/main.py`, `whatsapp.py` (appended section) | tool definitions and bridge client functions |
| Tests | `whatsapp-mcp-server/tests/test_groups.py`, `test_send_jobs.py` | + five rows in `test_bridge_auth.py` |

The typical flow for an assistant: the user names people → `resolve_contacts` → ask about
any ambiguous names (showing `avatar_url` thumbnails) → `create_group` with the chosen JIDs.

## MCP tools

| Tool | What it does |
|---|---|
| `resolve_contacts(names, max_matches=5, with_avatars=True)` | For each name returns `status` (`unique` / `ambiguous` / `none`), `matches` (jid, name, phone, avatar_url) and `total_matches`. Avatars are fetched only for ambiguous names and failures leave `avatar_url` null. |
| `get_profile_picture(jid, preview=True)` | Short-lived CDN URL of a contact's, group's or community's picture. `url` is null (with a `reason`) when there is no picture or it is hidden from you. |
| `create_group(name, participants, description=None, announce=None, locked=None, member_add=None, join_approval=None, community_jid=None)` | Creates a group. `announce=True` → only admins can post. `locked=True` → only admins edit group info. `member_add` is `"all_members"` or `"admins_only"`. `join_approval=True` → admins approve invite-link joiners. `community_jid` creates it inside that community (community admins only). Participants WhatsApp refused are returned in `failed_participants`. |
| `update_group_settings(group_jid, name=None, description=None, announce=None, locked=None, member_add=None, join_approval=None)` | Changes only the fields passed. With no fields it returns the current state. |
| `update_group_participants(group_jid, participants, action)` | `action` is `add`, `remove`, `promote` or `demote`; per-participant `ok` flags in `results`. |
| `get_group_invite_link(group_jid, reset=False)` | The group's (or community's) `https://chat.whatsapp.com/…` link and `code`. `reset=True` revokes the current link and returns a new one. Admins only. |
| `set_group_picture(group_jid, image_url=None, image_path=None, image_base64=None, remove=False)` | Sets the picture of a group, or of a community itself (pass the community's jid), from exactly one source; `remove=True` clears it. Any JPEG, PNG or WebP up to 10 MB is turned upright, centre-cropped, scaled to 640x640 and sent as JPEG. Returns `picture_id`, `is_community` and what was sent in `image`. Community pictures need community admin rights; group pictures need admin rights unless the group's info is open to all members (`locked=False`). |
| `list_communities(community_jid=None, include_sub_groups=None)` | Communities you belong to, each with `announcement_group` and `sub_groups` (`jid`, `name`, `is_announcement_group`, `joined`, `hidden`, `is_general_chat`, `size`). Without `community_jid` it is a quick overview listing the groups you have joined (`sub_groups_source: joined_only`); with one `community_jid` it shows that community's complete group list from WhatsApp (`server`). `include_sub_groups=True` fetches complete lists for every community, but see the rate-limit note below. |
| `list_community_participants(community_jid, with_names=True)` | Everyone in any of the community's groups, once each: `jid`, `phone`, `lid` and (with `with_names`) a `name` from the local contact store. Community admins only. |
| `create_community(name, description=None)` | Creates a community with you as admin. WhatsApp adds its announcement group automatically (returned as `announcement_group`); add groups with `create_group(community_jid=…)` or `update_community_groups`. |
| `update_community_groups(community_jid, group_jids, action)` | `action` is `link` (move existing groups into the community) or `unlink` (make them standalone again); per-group `ok` flags in `results`. Needs admin rights on the community and each group; the announcement group cannot be unlinked. |

Participants may be given as JIDs (`…@s.whatsapp.net`, `…@lid`) or as phone numbers with
country code in any formatting (`+1 (202) 555-1234`). Group names are limited to 100
characters by WhatsApp, counted in code points, so an emoji built from several code points
(❤️‍🔥 is four) counts as that many. Names are passed through byte-for-byte: only leading
and trailing whitespace is trimmed, and zero-width joiners and variation selectors are kept.

### Slow-send jobs

| Tool | What it does |
|---|---|
| `start_send_job(recipients, message_template, label=None, dry_run=False)` | Creates a job that messages each recipient once, in the background. `recipients` is a list of `{"recipient": <JID or phone>, "name": <name>}` (bare strings are fine without `{name}`). `{name}` in the template is replaced per person; any other `{placeholder}` is rejected. Returns the job plus `duplicates_removed`. `dry_run=True` paces and records progress but sends nothing. |
| `get_send_job(job_id, recipient_status=None)` | Status, counts and every recipient's `status` (`pending` / `sending` / `sent` / `failed`) with `attempts`, `last_attempt_at`, `sent_at` and `error`. For the running job: `waiting_for`, `next_send_at`, `estimated_remaining_seconds`. |
| `list_send_jobs(status=None, limit=20)` | Newest first, without recipients, plus the bridge's pacing settings. |
| `pause_send_job(job_id)` | Stops sending until resumed (a send already in flight completes). |
| `resume_send_job(job_id, retry_failed=False)` | Continues with pending recipients only; `retry_failed=True` also retries failed ones, once each (works on a completed job with failures too). |
| `cancel_send_job(job_id)` | Ends the job for good; pending recipients are never sent. |

How it behaves:

- **Runs in the bridge**, not the MCP server, so a job keeps going when the chat session
  ends, and every MCP client sees the same jobs. State lives in its own SQLite file,
  `whatsapp-bridge/store/send_jobs.db` (gitignored with the rest of `store/`).
- **Pacing** (account-wide, persisted across restarts): a random gap of
  `WHATSAPP_SEND_JOB_MIN_DELAY_S`–`WHATSAPP_SEND_JOB_MAX_DELAY_S` (45–90 s) between sends,
  and after every `WHATSAPP_SEND_JOB_BATCH_SIZE` (25) sends a gap of
  `WHATSAPP_SEND_JOB_BATCH_BREAK_S` (300 s) instead. A quiet spell at least as long as a
  break counts as one. 60 people take about 75 minutes. There is no daily cap.
- **One job at a time**, oldest first; a newer job shows as `queued` behind it, so two jobs
  never double the send rate.
- **At most once per recipient.** Recipients are de-duplicated on creation (phone formats,
  device suffixes, and an `@lid` the bridge can map to a phone number all count as the same
  person). Each one is claimed (`pending` → `sending`) in SQLite *before* the send and
  marked `sent`/`failed` after; a trigger makes `sent` final. A real job is refused if any
  of its people are in another unfinished (running or paused) real job.
- **Ceiling**: more than `WHATSAPP_SEND_JOB_MAX_RECIPIENTS` (100) unique recipients is
  rejected, never truncated.
- **Halt on failure**: a failed send marks that recipient `failed` and pauses the job with
  a `pause_reason`. A disconnected bridge is not a failure: the job waits
  (`waiting_for: whatsapp_connection`) and carries on once reconnected.
- **Restarts never re-send.** A recipient still `sending` at startup (the bridge stopped
  mid-send) becomes `sent` if `messages.db` holds that exact outgoing text from around the
  attempt, otherwise `failed` with an "unknown delivery, check the chat" error, and its job
  is paused. Nothing is retried automatically.
- **Dry runs**: `dry_run=True` per job, or `WHATSAPP_SEND_JOB_DRY_RUN=true` to force every
  new job to be a dry run. The flag is fixed when a job is created, so switching the env var
  later never turns a dry job real (or marks real recipients as sent without sending).
- Messages go through the same path as `send_message` (LID resolution, disappearing-message
  settings, stored in `messages.db` so they show up in `list_messages`).

## Bridge REST endpoints

All endpoints require the usual bearer token and return JSON with `success` and, on failure,
`message`. A bridge that is not logged in answers `503`; WhatsApp-side failures are `502`.

### `POST /api/group/create`

```json
{
  "name": "Trip",
  "participants": ["12025551234", "184125298348272@lid"],
  "description": "Optional",
  "announce": true,
  "locked": false,
  "member_add": "admins_only",
  "join_approval": false,
  "ephemeral_seconds": 0,
  "community_jid": "1203…@g.us"
}
```

Response: `{"success": true, "group": {…}, "warnings": {…}?, "failed_participants": […]?}`.
`announce`, `locked` and `join_approval` are sent in the create request itself;
`community_jid` goes in the create request too (as `linked_parent`), so the group is born
inside the community. `description` and `member_add` are applied right after (a short settle delay is paid only
when one of them is set). `warnings` maps a setting name to the error if it did not stick.
`failed_participants` lists people WhatsApp would not add, with `error`, an `error_hint`
(403 = privacy settings, 408 = recently left, 409 = already a member) and, for 403, an
`add_request` invite code.

### `POST /api/group/settings`

```json
{"group_jid": "1203…@g.us", "name": "…", "description": "…", "announce": true,
 "locked": true, "member_add": "all_members", "join_approval": true}
```

Only the fields present are changed. Response: `{"success": <all applied>, "group": {…}, "warnings"?: {…}}`.

### `POST /api/group/participants`

```json
{"group_jid": "1203…@g.us", "participants": ["12025551234"], "action": "add"}
```

Response: `{"success": <all ok>, "action": "add", "results": [{"jid", "ok", "error"?, "error_hint"?, "add_request"?}]}`.

### `POST /api/group/invite-link`

```json
{"group_jid": "1203…@g.us", "reset": false}
```

Response: `{"success": true, "group_jid", "link": "https://chat.whatsapp.com/…", "code",
"reset"}`. Without `reset` the link is only read; `reset: true` revokes it and issues a new
one. It is a POST either way so the destructive variant cannot be hit by a stray GET.
Errors: `403` (not an admin, or not in the group), `404` (no such group), `502` otherwise.

### `POST /api/group/picture`

```json
{"group_jid": "1203…@g.us", "image_url": "https://…"}
{"group_jid": "1203…@g.us", "image_path": "/abs/path/in/media/roots/pic.png"}
{"group_jid": "1203…@g.us", "image_base64": "iVBORw0…"}
{"group_jid": "1203…@g.us", "remove": true}
```

Exactly one image source, or `remove: true` with none. `group_jid` may be a group or a
community (its own picture). Response: `{"success": true, "group_jid", "is_community"
(null if the group info could not be read), "removed", "picture_id" (null after a removal),
"image"?: {"source", "format", "original_width", "original_height", "width": 640, "height":
640, "bytes"}}`.

- **Conversion.** JPEG, PNG and WebP are accepted (GIF and others are not). The image is
  turned upright by its EXIF orientation (JPEG), centre-cropped to a square, scaled up or
  down to 640x640 (Catmull-Rom), flattened onto white where transparent and encoded as
  JPEG at quality 85, which is what WhatsApp's own clients upload. Input is capped at
  10 MB and 50 megapixels; the dimensions are checked before any pixels are decoded.
- **`image_url`** must be `https`. The download has a 15 s timeout, at most 3 redirects
  (all `https`), and the response must say `image/jpeg`, `image/png` or `image/webp`. The
  bridge refuses to connect to loopback, private, link-local, carrier-grade-NAT (which
  includes Tailscale) and other non-public addresses; the check runs on the resolved IP of
  every connection, so DNS tricks and redirects cannot reach local services.
- **`image_path`** is confined to `WHATSAPP_MEDIA_ROOTS`, exactly like `/api/send`.
- **`image_base64`** may be standard or URL-safe base64, with or without padding or line
  breaks, or a `data:image/…;base64,` URL. The request body is capped at about 14 MB.
- **Permissions.** Before writing, the bridge reads the group: a known non-admin of a
  community, or of a group whose info is locked to admins, gets `403` without anything
  being sent. When the info does not say (WhatsApp lists only some of a community's
  members in it), WhatsApp decides and a refusal is also `403` with the same message.

Errors: `400` (bad input or an image that cannot be used), `413` (over 10 MB), `403` (not
allowed, or not in the group), `404` (no such group), `429` (rate-limited), `502` otherwise.

### `GET /api/profile-picture?jid=<jid|phone|group>&preview=1`

Response: `{"success": true, "jid": "…", "url": "https://…" | null, "id"?, "type"?, "reason"?: "not_set" | "hidden" | "unchanged"}`.

For a community the plain picture query answers `401` ("hidden") to anyone who is not in
the community's parent group itself, which is most members (seen live: the community
admin's read works, ordinary members get `hidden`). On a `401` for an `@g.us` JID the bridge
therefore retries with whatsmeow's community query (`IsCommunity`, a `w:g2` IQ) and uses
that answer when it is definite.

### `GET /api/communities?community_jid=<jid>&sub_groups=0|1`

Response: `{"success": true, "communities": [{"jid", "name", "description", "you_are_admin",
"announcement_group": {"jid", "name", "joined"} | null, "sub_groups": [{"jid", "name",
"is_announcement_group", "joined", "hidden", "is_general_chat", "size"}],
"sub_groups_source": "server" | "joined_only"}], "warnings"?: {"<community jid>": "…"}}`.
One IQ lists every joined group. The complete
group list is one more IQ per community; it is fetched by default only when
`community_jid` names a single community (`404` if you are not in it), or for every
community with `sub_groups=1`. `you_are_admin` is `null` when WhatsApp did not include you
in the community's participant list.

**Rate limit:** WhatsApp answers a burst of `sub_groups` queries with `429 rate-overlimit`
(seen live after nine in a row). The bridge stops at the first 429 and marks the remaining
communities as skipped in `warnings` (they fall back to `joined_only`) rather than keep
hitting a limited account.

The complete list is the same `<sub_groups/>` IQ as whatsmeow's `GetSubGroups`, sent through
`DangerousInternals().SendGroupIQ` so the whole response can be parsed: whatsmeow keeps
only `jid`, `subject` and `<default_sub_group/>`, but each `<group>` also carries `size`
and, live, the child flags `<general_chat/>` and `<hidden_group/>`. For `joined_only`
entries `hidden` and `is_general_chat` are `null` (unknown).

### Hidden groups: read-only

A community admin can hide a group from the community's group list (only invited members
see it). What this fork can and cannot do with that:

- **Read: yes.** `<hidden_group/>` appears on hidden groups in the `sub_groups` response
  (verified on the live account), exposed as `hidden`.
- **Set at create time: not built.** WhatsApp only offers the choice when a group is
  created inside a community. whatsmeow's `ReqCreateGroup` has no field for it and the
  create-time encoding is undocumented. By analogy with `<locked/>` and `<announcement/>`
  it is probably a `<hidden_group/>` child of `<create>`, but that cannot be verified
  without creating a real group, and a wrong guess would silently create a *visible*
  group the user believes is hidden. App-only until it can be tested on a throwaway
  community.
- **Change afterwards: not possible**, in the app or the protocol. WhatsApp does not let
  an existing community group's visibility be changed.

### `GET /api/community/participants?community_jid=<jid>`

Response: `{"success": true, "community_jid", "count", "participants": [{"jid", "phone",
"lid"}]}` from the `linked_groups_participants` IQ. `phone`/`lid` are filled from the local
LID map where known and `null` otherwise. The MCP server adds `name`. WhatsApp answers
`403 forbidden` unless you are a community admin (verified live), reported as `403` "only
community admins can list a community's members"; an unknown community is `404`.

### `POST /api/community/create`

```json
{"name": "Forest", "description": "Optional"}
```

Response: `{"success": true, "community": {…group object, "is_community": true…},
"announcement_group": {"jid", "name"} | null, "warnings"?: {…}}`. The create IQ carries
`<parent default_membership_approval_mode="request_required"/>` and no participants; the
server creates the announcement group. After the settle delay the description (if any) is
applied and the new community's groups are listed to find the announcement group. Failures
after the create show up in `warnings`.

### `POST /api/community/groups`

```json
{"community_jid": "1203…@g.us", "group_jids": ["1203…@g.us"], "action": "link"}
```

`action` is `link` or `unlink`; one IQ per group. Response: `{"success": <all ok>, "action",
"community_jid", "results": [{"jid", "ok", "error"?}]}`.

### Send jobs

| Endpoint | Body / query | Notes |
|---|---|---|
| `POST /api/send-jobs` | `{"recipients": [{"recipient": "447700900123", "name": "Alice"}], "template": "Hi {name}…", "label": "…", "dry_run": false}` | `400` with `errors` (one per bad entry) for invalid input, `409` with `overlapping_recipients` when people are already in an unfinished job. |
| `GET /api/send-jobs` | `?status=running\|paused\|completed\|cancelled&limit=1..200` | Newest first, plus `pacing`. |
| `GET /api/send-jobs/{id}` | `?recipient_status=pending\|sending\|sent\|failed` | Job with `recipients`, `example_message` and `pacing`. |
| `POST /api/send-jobs/{id}/pause` | — | Idempotent; `409` for a completed or cancelled job. |
| `POST /api/send-jobs/{id}/resume` | `{"retry_failed": false}` (body optional) | `409` for a cancelled job, or a completed one with nothing to retry. |
| `POST /api/send-jobs/{id}/cancel` | — | `409` for a completed job. |

The send-job routes use Go 1.22 method patterns, so a wrong method gets the mux's plain-text
`405`. If `store/send_jobs.db` cannot be opened, the rest of the bridge runs and these
routes answer `503`.

### Group object

```json
{"jid": "…@g.us", "name": "…", "description": "…", "description_id": "3EB0…",
 "announce": false, "locked": false, "member_add": "all_members", "join_approval": false,
 "is_community": false, "community_jid": "…@g.us" | null, "is_announcement_group": false,
 "participants": [{"jid", "phone", "lid", "is_admin", "is_super_admin"}]}
```

`is_community` is true for a community itself, `community_jid` names the community a group
belongs to, and `is_announcement_group` marks that community's announcement group.

`description_id` is WhatsApp's concurrency token for the description: every change must
quote the id it replaces (`prev`), and a mismatch is a `409 conflict` ("another member
changed the description recently" on the phone). The bridge sends it automatically; it is
exposed so a bad value (for example the literal string `undefined`) can be diagnosed.

## Notes

- **Community pictures are set like group pictures.** whatsmeow's `SetGroupPhoto` sends an
  iq set in `w:profile:picture` with `target` = the `@g.us` JID, and has no
  community-specific variant; whatsapp-rust does the same. A community admin's plain read of
  the community picture returns it from that same namespace, which is the evidence that the
  community's own picture lives there. Setting it was not tried on a live account (writes
  are unit-tested only); if WhatsApp ever refuses it for a community admin, the error comes
  back as `success: false` with WhatsApp's message.

- The bridge must be rebuilt (`go build`) and restarted for the new routes to exist; an old
  binary answers `404` for them and the MCP tools report that as `success: false` with the
  HTTP status in `message`.
- WhatsApp decides who can be added. People who set "Who can add me to groups" to
  "nobody" or "my contacts" get an invite (`add_request`) instead of being added.
- Outbound calls are not covered.
