# Fork additions

This fork of [`verygoodplugins/whatsapp-mcp`](https://github.com/verygoodplugins/whatsapp-mcp)
adds WhatsApp **group management** and **profile pictures**. Everything lives in new
files so the diff against upstream stays small:

| Component | File | Upstream touch |
|---|---|---|
| Bridge endpoints | `whatsapp-bridge/groups.go` (+ `groups_test.go`) | one `registerGroupRoutes(...)` line in `main.go` |
| MCP tools | `whatsapp-mcp-server/main.py`, `whatsapp.py` (appended section) | tool definitions and bridge client functions |
| Tests | `whatsapp-mcp-server/tests/test_groups.py` | + three rows in `test_bridge_auth.py` |

The typical flow for an assistant: the user names people → `resolve_contacts` → ask about
any ambiguous names (showing `avatar_url` thumbnails) → `create_group` with the chosen JIDs.

## MCP tools

| Tool | What it does |
|---|---|
| `resolve_contacts(names, max_matches=5, with_avatars=True)` | For each name returns `status` (`unique` / `ambiguous` / `none`), `matches` (jid, name, phone, avatar_url) and `total_matches`. Avatars are fetched only for ambiguous names and failures leave `avatar_url` null. |
| `get_profile_picture(jid, preview=True)` | Short-lived CDN URL of a contact's or group's picture. `url` is null (with a `reason`) when there is no picture or it is hidden from you. |
| `create_group(name, participants, description=None, announce=None, locked=None, member_add=None, join_approval=None)` | Creates a group. `announce=True` → only admins can post. `locked=True` → only admins edit group info. `member_add` is `"all_members"` or `"admins_only"`. `join_approval=True` → admins approve invite-link joiners. Participants WhatsApp refused are returned in `failed_participants`. |
| `update_group_settings(group_jid, name=None, description=None, announce=None, locked=None, member_add=None, join_approval=None)` | Changes only the fields passed. With no fields it returns the current state. |
| `update_group_participants(group_jid, participants, action)` | `action` is `add`, `remove`, `promote` or `demote`; per-participant `ok` flags in `results`. |

Participants may be given as JIDs (`…@s.whatsapp.net`, `…@lid`) or as phone numbers with
country code in any formatting (`+1 (202) 555-1234`). Group names are limited to 100
characters by WhatsApp, counted in code points, so an emoji built from several code points
(❤️‍🔥 is four) counts as that many. Names are passed through byte-for-byte: only leading
and trailing whitespace is trimmed, and zero-width joiners and variation selectors are kept.

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
  "ephemeral_seconds": 0
}
```

Response: `{"success": true, "group": {…}, "warnings": {…}?, "failed_participants": […]?}`.
`announce`, `locked` and `join_approval` are sent in the create request itself;
`description` and `member_add` are applied right after (a short settle delay is paid only
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

### `GET /api/profile-picture?jid=<jid|phone|group>&preview=1`

Response: `{"success": true, "jid": "…", "url": "https://…" | null, "id"?, "type"?, "reason"?: "not_set" | "hidden" | "unchanged"}`.

### Group object

```json
{"jid": "…@g.us", "name": "…", "description": "…", "description_id": "3EB0…",
 "announce": false, "locked": false, "member_add": "all_members", "join_approval": false,
 "participants": [{"jid", "phone", "lid", "is_admin", "is_super_admin"}]}
```

`description_id` is WhatsApp's concurrency token for the description: every change must
quote the id it replaces (`prev`), and a mismatch is a `409 conflict` ("another member
changed the description recently" on the phone). The bridge sends it automatically; it is
exposed so a bad value (for example the literal string `undefined`) can be diagnosed.

## Notes

- The bridge must be rebuilt (`go build`) and restarted for the new routes to exist; an old
  binary answers `404` for them and the MCP tools report that as `success: false` with the
  HTTP status in `message`.
- WhatsApp decides who can be added. People who set "Who can add me to groups" to
  "nobody" or "my contacts" get an invite (`add_request`) instead of being added.
- Outbound calls, communities and invite-link management are not covered.
