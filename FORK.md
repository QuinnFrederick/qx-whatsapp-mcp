# Fork additions

This fork of [`verygoodplugins/whatsapp-mcp`](https://github.com/verygoodplugins/whatsapp-mcp)
adds WhatsApp **group management**, **communities** and **profile pictures**. Everything
lives in new files so the diff against upstream stays small:

| Component | File | Upstream touch |
|---|---|---|
| Bridge endpoints | `whatsapp-bridge/groups.go`, `communities.go` (+ `_test.go`) | one `registerGroupRoutes(...)` line in `main.go` (it registers the community routes too) |
| MCP tools | `whatsapp-mcp-server/main.py`, `whatsapp.py` (appended section) | tool definitions and bridge client functions |
| Tests | `whatsapp-mcp-server/tests/test_groups.py` | + three rows in `test_bridge_auth.py` |

The typical flow for an assistant: the user names people → `resolve_contacts` → ask about
any ambiguous names (showing `avatar_url` thumbnails) → `create_group` with the chosen JIDs.

## MCP tools

| Tool | What it does |
|---|---|
| `resolve_contacts(names, max_matches=5, with_avatars=True)` | For each name returns `status` (`unique` / `ambiguous` / `none`), `matches` (jid, name, phone, avatar_url) and `total_matches`. Avatars are fetched only for ambiguous names and failures leave `avatar_url` null. |
| `get_profile_picture(jid, preview=True)` | Short-lived CDN URL of a contact's or group's picture. `url` is null (with a `reason`) when there is no picture or it is hidden from you. |
| `create_group(name, participants, description=None, announce=None, locked=None, member_add=None, join_approval=None, community_jid=None)` | Creates a group. `announce=True` → only admins can post. `locked=True` → only admins edit group info. `member_add` is `"all_members"` or `"admins_only"`. `join_approval=True` → admins approve invite-link joiners. `community_jid` creates it inside that community (community admins only). Participants WhatsApp refused are returned in `failed_participants`. |
| `update_group_settings(group_jid, name=None, description=None, announce=None, locked=None, member_add=None, join_approval=None)` | Changes only the fields passed. With no fields it returns the current state. |
| `update_group_participants(group_jid, participants, action)` | `action` is `add`, `remove`, `promote` or `demote`; per-participant `ok` flags in `results`. |
| `get_group_invite_link(group_jid, reset=False)` | The group's (or community's) `https://chat.whatsapp.com/…` link and `code`. `reset=True` revokes the current link and returns a new one. Admins only. |
| `list_communities(community_jid=None, include_sub_groups=None)` | Communities you belong to, each with `announcement_group` and `sub_groups` (`jid`, `name`, `is_announcement_group`, `joined`, `hidden`, `is_general_chat`, `size`). Without `community_jid` it is a quick overview listing the groups you have joined (`sub_groups_source: joined_only`); with one `community_jid` it shows that community's complete group list from WhatsApp (`server`). `include_sub_groups=True` fetches complete lists for every community, but see the rate-limit note below. |
| `list_community_participants(community_jid, with_names=True)` | Everyone in any of the community's groups, once each: `jid`, `phone`, `lid` and (with `with_names`) a `name` from the local contact store. Community admins only. |
| `create_community(name, description=None)` | Creates a community with you as admin. WhatsApp adds its announcement group automatically (returned as `announcement_group`); add groups with `create_group(community_jid=…)` or `update_community_groups`. |
| `update_community_groups(community_jid, group_jids, action)` | `action` is `link` (move existing groups into the community) or `unlink` (make them standalone again); per-group `ok` flags in `results`. Needs admin rights on the community and each group; the announcement group cannot be unlinked. |

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

### `GET /api/profile-picture?jid=<jid|phone|group>&preview=1`

Response: `{"success": true, "jid": "…", "url": "https://…" | null, "id"?, "type"?, "reason"?: "not_set" | "hidden" | "unchanged"}`.

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

- The bridge must be rebuilt (`go build`) and restarted for the new routes to exist; an old
  binary answers `404` for them and the MCP tools report that as `success: false` with the
  HTTP status in `message`.
- WhatsApp decides who can be added. People who set "Who can add me to groups" to
  "nobody" or "my contacts" get an invite (`add_request`) instead of being added.
- Outbound calls are not covered.
