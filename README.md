# qx-whatsapp-mcp

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Python 3.11+](https://img.shields.io/badge/python-3.11+-blue.svg)](https://www.python.org/downloads/)
[![Go 1.25+](https://img.shields.io/badge/go-1.25+-00ADD8.svg)](https://go.dev/)

A Model Context Protocol (MCP) server for WhatsApp, enabling Claude to read and send WhatsApp messages.

> [!WARNING]
> **Early, experimental software (version 0.1). Use at your own risk.**
> This is an unofficial project. It is not affiliated with, endorsed by or connected to
> WhatsApp or Meta. It signs in to your personal account as a linked device through an
> unofficial client library ([whatsmeow](https://github.com/tulir/whatsmeow)), which can
> break without notice, and WhatsApp may restrict accounts that use unofficial clients.

> Fork of [verygoodplugins/whatsapp-mcp](https://github.com/verygoodplugins/whatsapp-mcp)
> (itself a fork of [Luke Harries' original](https://github.com/lharries/whatsapp-mcp)) that adds
> group management and WhatsApp Communities. See [`FORK.md`](./FORK.md).

<p align="center">
  <a href="https://github.com/user-attachments/assets/9475af1d-2369-4315-9ccc-823dba2c5c32"><strong>Watch the WhatsApp MCP demo video</strong></a>
</p>

<p align="center">
  <sub>Product demo generated with Remotion using simulated data.</sub>
</p>

## Features

- **Message Management**: Search and read personal WhatsApp messages (text, images, videos, documents, audio)
- **Contact Search**: Search contacts by name or phone number with `sender_display` format ("Name (phone)")
- **Send Messages**: Send text messages to individuals or groups
- **Read Receipts**: Explicitly mark selected messages as read across linked devices
- **Media Support**: Send and download images, videos, documents, and voice messages
- **Call History**: Capture incoming voice/video calls into a local SQLite table (live, 1:1 and group)
- **Webhook Integration**: Forward incoming messages to external services
- **Local Storage**: All messages stored locally in SQLite - only sent to Claude when you allow it

## Getting started

You need git, Go 1.25+, [uv](https://docs.astral.sh/uv/), a C compiler (the SQLite driver
uses cgo: Xcode Command Line Tools on macOS, `build-essential` on Debian/Ubuntu) and Claude
Code or Claude Desktop. FFmpeg is optional (voice notes). uv fetches Python 3.11+ if you
don't have it.

### 1. Run the bridge and install the MCP server

```bash
git clone -b feature/group-management https://github.com/QuinnFrederick/qx-whatsapp-mcp.git
cd qx-whatsapp-mcp/whatsapp-bridge
go run .
```

✅ The first run compiles for a minute or two, then prints a `WHATSAPP BRIDGE AUTH TOKEN —
first-time setup` box and a QR code. Leave it running. The token is saved to
`store/.bridge-token`; the MCP server reads it from there, so there is nothing to copy.

In a second terminal:

```bash
cd qx-whatsapp-mcp/whatsapp-mcp-server
uv sync
```

✅ Finishes without errors. You don't start the MCP server yourself; Claude launches it (step 3).

### 2. Link your WhatsApp account

On your phone open WhatsApp → **Settings → Linked devices → Link a device** (Android: ⋮ menu →
**Linked devices**) and scan the QR code in the bridge terminal. Scan within about a minute:
the bridge prints only the first code.

✅ The bridge prints:

```text
Successfully connected and authenticated!
✓ Connected to WhatsApp! Type 'help' for commands.
REST server is running. Press Ctrl+C to disconnect and exit.
```

and your phone lists a new linked device. Chat history then syncs in the background: recent
chats within minutes, longer for big accounts. Check the bridge from another terminal (its API
only starts after pairing):

```bash
cd qx-whatsapp-mcp/whatsapp-bridge
curl -s -H "Authorization: Bearer $(cat store/.bridge-token)" http://127.0.0.1:8080/api/health
```

✅ `{"connected":true,"status":"ok",...}`

The link persists: later runs of `go run .` reconnect without a QR code. **To re-pair** (a
different account, after unlinking the device on your phone, or to fetch more history), stop
the bridge with Ctrl+C and move the session aside:

```bash
mv store/whatsapp.db store/whatsapp.db.bak
go run .
```

A new QR code appears; `store/messages.db`, your local message archive, is kept. To name the
device in your phone's list, pair with `WHATSAPP_DEVICE_NAME="My laptop" go run .`. To ask the
phone for more than its default ~3 months of history, pair with `go run . --full-history-pair`
([details](#requesting-full-history)).

### 3. Connect it to Claude

**Claude Code**: from `qx-whatsapp-mcp/whatsapp-mcp-server`:

```bash
claude mcp add --scope user whatsapp -- uv --directory "$PWD" run main.py
```

✅ `claude mcp list` shows `whatsapp` as connected.

**Claude Desktop**: print the two absolute paths you need:

```bash
cd qx-whatsapp-mcp/whatsapp-mcp-server && pwd && which uv
```

Open **Settings → Developer → Edit Config** (`~/Library/Application Support/Claude/claude_desktop_config.json`
on macOS, `%APPDATA%\Claude\claude_desktop_config.json` on Windows) and add, with your paths:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/absolute/path/to/uv",
      "args": ["--directory", "/absolute/path/to/qx-whatsapp-mcp/whatsapp-mcp-server", "run", "main.py"]
    }
  }
}
```

Use the full path to `uv`: Claude Desktop doesn't inherit your shell's `PATH`. Quit Claude
Desktop completely and reopen it.

✅ `whatsapp` and its tools appear in the chat box's tools menu.

Cursor: see [Cursor IDE Configuration](#cursor-ide-configuration). The claude.ai web and mobile
apps need a remote HTTPS connector; this server runs locally over stdio by default (see
[MCP transport](#mcp-transport-stdio-vs-httpsse)).

### 4. Try it

Ask Claude:

> Which of my WhatsApp chats have unread messages? Summarise them.

✅ Claude asks to use `list_chats` and `list_messages`, then answers with your real chats. If
it finds none, history is still syncing; try again in a few minutes.

Reading is safe to experiment with. Sending reaches real people, so try it on yourself first:
"Send 'hello from Claude' to +<your number>" should land in your own "message yourself" chat.
Then try "List my WhatsApp communities and their groups."

### Troubleshooting setup

- **The QR code expired before you scanned it.** Only the first code is printed and the next
  attempt is minutes away: press Ctrl+C and run `go run .` again.
- **The QR code is garbled.** It is drawn with Unicode block characters: widen the terminal or
  shrink the font.
- **`Client outdated` or HTTP 405 while pairing.** WhatsApp raised its minimum client version:
  `git pull`, then `go run .` again.
- **`cgo: C compiler "gcc" not found` or `go-sqlite3 requires cgo`.** Install a C compiler (see
  the start of this section; on Windows see [Windows](#windows)).
- **Claude doesn't list the WhatsApp tools.** Check the paths are absolute (including `uv` for
  Claude Desktop), restart Claude fully, and read the server log
  (`~/Library/Logs/Claude/mcp-server-whatsapp.log` for Claude Desktop on macOS).
- **Sending fails with `Request error … Connection refused`.** The bridge isn't running or isn't
  paired yet. Reading still works, because it uses the local database.
- **Tools fail with `HTTP 401`.** The MCP server and the bridge are using different tokens, which
  happens when they aren't in the same checkout. Set `WHATSAPP_BRIDGE_TOKEN` to the contents of
  `whatsapp-bridge/store/.bridge-token` for the MCP server (`-e` with `claude mcp add`, an
  `"env"` block in the Desktop config).
- **Chats or older messages are missing.** Sync may still be running, and phones share only
  recent history by default. See [Requesting full history](#requesting-full-history) and
  [single-chat history](#requesting-history-for-a-single-chat-on-demand).
- **"Device limit reached" on the phone.** Unlink an old device under Linked devices.

More in [Troubleshooting](#troubleshooting).

### Updating

Pull the latest changes, then refresh whichever components moved:

```bash
git pull
```

| You changed                                                              | What to do                                                                                                                                            |
| ------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Bridge code** (`whatsapp-bridge/*.go`) and you run `go run .`          | Nothing — `go run` recompiles each launch. Just restart the bridge.                                                                                   |
| **Bridge code** and you run a built binary                               | `cd whatsapp-bridge && go build -o whatsapp-bridge && ./whatsapp-bridge`                                                                              |
| **MCP server** (`whatsapp-mcp-server/*.py`, `pyproject.toml`, `uv.lock`) | Restart Claude Desktop / Cursor — `uv` re-resolves from the lockfile on next launch. Force a sync with `cd whatsapp-mcp-server && uv sync` if needed. |

Updates do **not** require re-pairing or deleting `whatsapp.db` — your session and message history are preserved. Re-pairing is only needed when explicitly requesting full history (see [Requesting full history](#requesting-full-history)).

For `v0.2.1` and later, restart both the bridge and MCP server after updating
so the MCP server can read the bridge token. If the two components do not share
the same checkout, set the same `WHATSAPP_BRIDGE_TOKEN` value in both
environments.

### Cursor IDE Configuration

Add to your Cursor MCP settings (`~/.cursor/mcp.json`):

```json
{
  "mcp": {
    "servers": {
      "whatsapp": {
        "command": "uv",
        "args": [
          "--directory",
          "/path/to/whatsapp-mcp/whatsapp-mcp-server",
          "run",
          "main.py"
        ]
      }
    }
  }
}
```

## Tools

Messages include `sender_display` showing "Name (phone)" format for easy identification by agents.

### Contact Operations

#### `search_contacts`

Search contacts by name or phone number.

**Parameters:**

- `query` (required): Name or phone number to search

**Natural Language Examples:**

- "Find contacts named John"
- "Search for phone number 555-1234"
- "Who has the phone number starting with +1?"

#### `get_contact`

Resolve a WhatsApp contact name from a phone number, LID, or full JID.

**Parameters:**

- `identifier` (required): Phone number, LID, or full JID (aliases: `phone_number`, `phone`)
  - Examples: `12025551234`, `184125298348272`, `12025551234@s.whatsapp.net`, `184125298348272@lid`

**Natural Language Examples:**

- "What's the name for phone number 5551234567?"
- "Look up who owns this number"
- "Who is 184125298348272@lid?"

### Message Operations

#### `list_messages`

Get messages with filters, date ranges, and sorting.

**Parameters:**

- `chat_jid` (optional): Filter by specific chat JID
- `limit` (optional): Number of messages (default 50, max 500)
- `before_date` (optional): Messages before this date (YYYY-MM-DD)
- `after_date` (optional): Messages after this date (YYYY-MM-DD)
- `sort_by` (optional): "newest" or "oldest" (default "newest")

**Natural Language Examples:**

- "Show me the last 100 messages from today"
- "Get messages from the family group chat"
- "Find messages from last week"

#### `send_message`

Send a text message to a contact or group, optionally as a quoted reply.

**Parameters:**

- `recipient` (required): Phone number or group JID
- `message` (required): Text content to send
- `quoted_message_id` (optional): ID of the message to reply to. When provided, the sent message appears as a quoted reply in WhatsApp.
- `quoted_sender_jid` (optional): Full JID of the author of the quoted message. Required for group replies so WhatsApp renders the correct attribution header.
- `quoted_content` (optional): Text content of the quoted message, used for the reply preview. Only plain text is supported.
- `mentions` (optional): List of users to @-mention, as phone numbers with country code (e.g. `["12025551234"]`) or JIDs. For each entry the message text must contain a matching `@<number>` token (e.g. `"thanks @12025551234!"`), which recipients' devices render as a highlighted, tappable mention that also notifies the user. Only meaningful in group chats.

Inbound quoted replies are stored automatically. The `quoted_message_id` field in each message returned by `list_messages` indicates which message it is replying to (or `null` for non-replies).

**Natural Language Examples:**

- "Send 'Hello!' to +1234567890"
- "Message the team group saying 'Meeting at 3pm'"
- "Reply to that message saying 'Sounds good'"

#### `mark_messages_read`

Mark one or more messages from the same chat and sender as read. This explicitly
sends WhatsApp read receipts; reading or searching messages never does so
automatically.

**Parameters:**

- `message_ids` (required): IDs of messages from the same chat and sender
- `chat_jid` (required): JID of the chat containing the messages
- `sender_jid` (required for groups): Full JID or bare phone number of the original message sender
- `timestamp` (optional): RFC 3339 read timestamp; defaults to the current time

**Natural Language Examples:**

- "Mark those messages as read"
- "Mark the last three messages from Alice in the team group as read"

#### `send_reaction`

Send (or remove) an emoji reaction to a message.

**Parameters:**

- `recipient` (required): Chat JID the message belongs to (phone JID or group JID)
- `message_id` (required): ID of the message to react to
- `emoji` (required): Reaction emoji (e.g. `"👍"`). Pass an empty string `""` to remove an existing reaction.
- `from_me` (optional, default `false`): Whether the original message was sent by the current user
- `sender_jid` (optional): Full JID of the original message sender — required for group messages when `from_me` is `false` so the correct WhatsApp key is built

Inbound reactions received from others are stored automatically as messages with `media_type = "reaction"`. The `reaction_to_message_id` field in each reaction message indicates which message was reacted to.

When webhook forwarding is enabled, inbound reactions are also posted to `WEBHOOK_URL` as typed events. Reaction removals use an empty `content`/`reactionEmoji` and `reactionRemoved: true`.

```json
{
  "eventType": "reaction",
  "sender": "15551234567",
  "chatJID": "15551234567@s.whatsapp.net",
  "isFromMe": true,
  "content": "👍",
  "messageId": "reaction-stanza-id",
  "mediaType": "reaction",
  "reactionToMessageId": "target-message-id",
  "reactionEmoji": "👍",
  "reactionRemoved": false
}
```

**Natural Language Examples:**

- "React to that message with a thumbs up"
- "Remove my reaction from the last message in the group chat"

#### `send_file`

Send a media file (image, video, document).

**Parameters:**

- `recipient` (required): Phone number or group JID
- `file_path` (required): Path to the file
- `caption` (optional): Caption for the media

The bridge only reads files inside configured media roots. By default this is
`~/.local/share/whatsapp-mcp/outbox`; set `WHATSAPP_MEDIA_ROOTS` to allow
additional absolute directories.

#### `send_audio_message`

Send a voice message (automatically converts to Opus .ogg format).

**Parameters:**

- `recipient` (required): Phone number or group JID
- `file_path` (required): Path to audio file

Converted audio is sent through the same media-path confinement as
`send_file`.

#### `download_media`

Download media from a received message.

**Parameters:**

- `message_id` (required): ID of the message with media
- `chat_jid` (required): JID of the chat containing the message

### Group Operations (fork)

This fork adds group management, WhatsApp Communities and profile pictures:
`resolve_contacts` (turn spoken names into contacts and flag ambiguous ones, with avatar
thumbnails), `get_profile_picture`, `create_group` (optionally inside a community),
`update_group_settings`, `update_group_participants`, `get_group_invite_link`,
`set_group_picture` (set or remove a group's or community's picture from a URL, a local
file or base64; any JPEG, PNG or WebP is cropped and converted for you),
`list_communities`, `list_community_participants`, `create_community` and
`update_community_groups`. Tool parameters, the bridge endpoints behind them
(`/api/group/*`, `/api/community/*`, `/api/communities`, `/api/profile-picture`), their JSON
shapes and what WhatsApp does not allow are documented in [`FORK.md`](./FORK.md).

### Chat Operations

All chat tools (`list_chats`, `get_chat`, `get_direct_chat_by_contact`,
`get_contact_chats`) return the same chat shape:

```jsonc
{
  "jid": "1234567890@s.whatsapp.net",
  "name": "Alice",
  "is_group": false,
  "last_message_time": "2024-01-15T10:30:00+00:00",
  "last_message": "hello world",       // null when include_last_message=false
  "last_sender": "1234567890",         // null when include_last_message=false
  "last_is_from_me": false,
  "last_read_time": "2024-01-15T09:00:00+00:00", // how far the chat is read
  "unread": true                       // last message is inbound and unread
}
```

#### Read state (`last_read_time` / `unread`)

`last_read_time` is the bridge's read marker for the chat, fed by read
receipts from your own devices and backfilled from history sync. `unread` is
derived from it: true when the chat's last message is inbound and newer than
the marker. This distinguishes a genuinely unread chat from one whose last
message merely happens to be inbound but was already read on the phone.

Caveats:

- **The marker only moves forward.** Marking an already-read chat as *unread*
  again on the phone is not reflected.
- **No marker means no read was ever reported** — for a chat with an inbound
  last message, `unread` then falls back to the old heuristic and reports
  true. Stores written by a bridge older than the `chats.last_read_time`
  column report `last_read_time: null` and behave the same way.
- **`unread` is a chat-level flag, not an unread count.** WhatsApp's unread
  counter is not persisted.

#### `list_chats`

List all chats with metadata.

**Parameters:**

- `limit` (optional): Number of chats (default 50, max 200)

#### `get_chat`

Get specific chat metadata by JID.

**Parameters:**

- `jid` (required): Chat JID

#### `get_direct_chat_by_contact`

Find a direct message chat with a contact.

**Parameters:**

- `phone` (required): Phone number of the contact

#### `get_contact_chats`

List all chats involving a specific contact.

**Parameters:**

- `phone` (required): Phone number of the contact

#### `get_last_interaction`

Get the last message exchanged with a contact.

**Parameters:**

- `phone` (required): Phone number of the contact

#### `get_message_context`

Get messages around a specific message for context.

**Parameters:**

- `message_id` (required): ID of the target message
- `chat_jid` (required): JID of the chat
- `before` (optional): Number of messages before (default 5)
- `after` (optional): Number of messages after (default 5)

## Configuration

Copy `.env.example` to `.env` and configure as needed:

| Variable               | Default                                  | Description                                  |
| ---------------------- | ---------------------------------------- | -------------------------------------------- |
| `WHATSAPP_BRIDGE_PORT` | `8080`                                   | Port for Go bridge REST API                  |
| `WEBHOOK_URL`          | `http://localhost:8769/whatsapp/webhook` | Webhook for incoming messages                |
| `WEBHOOK_ENABLED`      | `true`                                   | Set to `false` to disable outbound webhooks  |
| `FORWARD_SELF`         | `true`                                   | Forward messages sent by self                |
| `WHATSAPP_DB_PATH`     | `../whatsapp-bridge/store/messages.db`   | Path to SQLite database                      |
| `WHATSMEOW_DB_PATH`    | `../whatsapp-bridge/store/whatsapp.db`   | whatsmeow DB used for LID ↔ phone resolution |
| `WHATSAPP_API_URL`     | `http://localhost:8080/api`              | Go bridge REST API URL                       |
| `WHATSAPP_BRIDGE_TOKEN` | generated next to `WHATSMEOW_DB_PATH` as `.bridge-token` | Bearer token for bridge REST calls; also signed onto outbound webhook POSTs |
| `WHATSAPP_MEDIA_ROOTS` | `~/.local/share/whatsapp-mcp/outbox`     | Path-list of directories allowed for outbound media files |
| `WHATSAPP_DEVICE_NAME` | `whatsmeow` (whatsmeow default)          | Label shown for this connection under WhatsApp > Linked Devices. Set to a recognisable name. Applies at pair time only (re-pair to change) |
| `WHATSAPP_MCP_TRANSPORT` | `stdio`                                | MCP transport to serve clients: `stdio`, `http`, or `sse` |
| `WHATSAPP_MCP_HOST`    | `127.0.0.1`                              | Bind address for the `http`/`sse` transports |
| `WHATSAPP_MCP_PORT`    | `8000`                                   | Port for the `http`/`sse` transports |
| `WHATSAPP_PARENT_WATCHDOG_S` | `30`                              | Stdio parent-liveness poll interval (seconds); exits on parent reparent only |

### MCP transport (stdio vs http/sse)

By default the server speaks MCP over **stdio**, which is what local clients
like Claude Desktop and Cursor launch. To serve the server over the network
instead, set `WHATSAPP_MCP_TRANSPORT`:

```bash
# Streamable HTTP (current spec transport for remote MCP), endpoint at /mcp
WHATSAPP_MCP_TRANSPORT=http WHATSAPP_MCP_PORT=8000 uv run main.py

# Legacy Server-Sent Events transport (deprecated in the MCP spec), endpoint at /sse
WHATSAPP_MCP_TRANSPORT=sse uv run main.py
```

`http` is an alias for the spec's `streamable-http` transport and is the
recommended choice for remote connections; `sse` is kept for older clients.

> **Security:** `WHATSAPP_MCP_HOST` defaults to `127.0.0.1`, so the HTTP/SSE
> server is reachable only from the local machine. The server has no built-in
> authentication, and the underlying bridge can read and send WhatsApp messages
> on your account. Only bind to a non-loopback address (e.g. `0.0.0.0`) if you
> place an authenticating reverse proxy or tunnel in front of it.

### Bridge authentication and media paths

The bridge requires bearer-token authentication for every `/api/*` request and
accepts only exact loopback Host headers for its configured port. This protects
the local REST API from other local processes and browser DNS-rebinding attacks.

On first start, the bridge generates a 256-bit token, writes it to
`.bridge-token` in the active bridge store directory with owner-only
permissions, and prints a setup banner. The MCP server reads
`WHATSAPP_BRIDGE_TOKEN` first, then falls back to `.bridge-token` in the same
directory as `WHATSMEOW_DB_PATH`. For split deployments, containers, or process
managers that do not share the store directory, set the same
`WHATSAPP_BRIDGE_TOKEN` value for both the bridge and MCP server.

The bridge also signs its **outbound** webhook POSTs (to `WEBHOOK_URL`) with this
same token, sent as an `X-Bridge-Token: <token>` header — a dedicated header
rather than `Authorization`, so it never collides with a receiver's own
Authorization-based auth (e.g. HTTP Basic auth embedded in `WEBHOOK_URL` as
`http://user:pass@host/...`, which `net/http` applies automatically as long as
the bridge doesn't set its own `Authorization` header). The header is attached only when a token is configured **and** `WEBHOOK_URL` was
explicitly set — never to the built-in local default. The bridge token also
authorizes `/api/*` calls like sending messages, and nothing has vetted the
implicit default address, so it must never be handed to whatever process
happens to be listening there. Upgrades that predate the token rollout, or
that never set `WEBHOOK_URL`, keep working unchanged. The webhook client also
never follows redirects, so a misconfigured or malicious endpoint can't
redirect the bridge into leaking the token to a different host. If your
webhook receiver enforces the token, set its copy to this exact value: e.g.
the AutoHub hub's `WHATSAPP_BRIDGE_TOKEN` must equal this bridge's token (from
`.bridge-token` or its own env) — the hub accepts it via `X-Bridge-Token` or
`Authorization: Bearer`. The bridge always sends the token it has; the hub
rejects unauthenticated forwards only once its `WHATSAPP_BRIDGE_TOKEN` is set
to the matching value.

Outbound `media_path` values are confined to `WHATSAPP_MEDIA_ROOTS`. The default
outbox is `~/.local/share/whatsapp-mcp/outbox`, created on bridge startup. Move
files there before calling `send_file`, `send_audio_message` or
`set_group_picture(image_path=...)`, or set
`WHATSAPP_MEDIA_ROOTS` to a colon-separated list of absolute directories.

### Run automatically on macOS

macOS users can install optional per-user `launchd` jobs that start the Go
bridge at login and monitor it every 60 seconds for API health, disconnects, and
QR relink signals. The installer does not require `sudo` and does not install or
start the MCP server.

```bash
scripts/install-launchd-macos.sh
```

The installer builds `whatsapp-bridge/whatsapp-bridge` with `go build` when Go is
available, writes generated support files to
`~/Library/Application Support/whatsapp-mcp/`, writes LaunchAgents to
`~/Library/LaunchAgents/`, and writes logs to `~/Library/Logs/whatsapp-mcp/`.
It safely reloads only these labels:

- `com.whatsapp-mcp.bridge`
- `com.whatsapp-mcp.bridge-monitor`

To customize the launchd environment, export values before running the installer.
Re-run the installer after changing them.

```bash
export WHATSAPP_BRIDGE_PORT=8080
export WEBHOOK_URL=http://localhost:8769/whatsapp/webhook
export FORWARD_SELF=false
export WHATSAPP_MEDIA_ROOTS="$HOME/.local/share/whatsapp-mcp/outbox"
scripts/install-launchd-macos.sh
```

Verify the jobs and inspect logs:

```bash
launchctl print gui/$(id -u)/com.whatsapp-mcp.bridge
launchctl print gui/$(id -u)/com.whatsapp-mcp.bridge-monitor
tail -n 100 ~/Library/Logs/whatsapp-mcp/bridge.err.log
tail -n 100 ~/Library/Logs/whatsapp-mcp/monitor.err.log
```

The monitor sends a macOS notification once per failure type until recovery. It
alerts when the bridge LaunchAgent is unloaded, the token is missing, the health
endpoint is unreachable, WhatsApp is disconnected, or recent logs indicate that
QR relinking is needed.

Uninstall the generated LaunchAgents and support files with:

```bash
scripts/uninstall-launchd-macos.sh
```

Uninstall preserves `whatsapp-bridge/store/`, including WhatsApp session DBs,
message DBs, media, and `.bridge-token`. Logs are left in
`~/Library/Logs/whatsapp-mcp/` for manual cleanup.

### CLI flags (Go bridge)

| Flag                  | Default | Description                                                                                                                                                                                                                                                       |
| --------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--full-history-pair` | `false` | Request full history at pair time. Only takes effect on a fresh pair (no existing `whatsapp.db`); no-op for already-paired sessions. The phone ultimately decides the actual history window sent — see [Requesting full history](#requesting-full-history) below. |

### Requesting full history

whatsmeow's default pairing asks for "recent sync" — roughly the last 3 months, with the exact window decided by the phone. If you want to pull more history at pair time:

```bash
# Stop the bridge
launchctl bootout gui/$UID/com.whatsapp-mcp.bridge    # or however you manage it

# Back up, then remove the auth session (keeps messages.db intact)
cp whatsapp-bridge/store/whatsapp.db{,.bak}
rm whatsapp-bridge/store/whatsapp.db

# Re-pair with the flag
cd whatsapp-bridge
./whatsapp-bridge --full-history-pair
# Scan the QR with WhatsApp → Settings → Linked Devices → Link a Device
# Wait for "History sync complete" in the logs (can take 10-30 minutes)
# Ctrl+C when sync has quiesced, then restart under your normal process manager
```

Caveats:

- **The phone decides the actual cap.** The flag requests up to 10 years / 100 GB, but WhatsApp's iOS primary device enforces its own retention policy. iPad companion is documented at ~1 year max; other linked devices appear to follow similar logic.
- **Only effective on a fresh pair.** With `whatsapp.db` already present, no new pair handshake fires and the flag is a no-op.
- **Messages the phone has deleted are not recoverable** — auto-expire, low-storage cleanup, and manual delete all leave no trace for the phone to share.

### Requesting history for a single chat (on-demand)

`--full-history-pair` only applies to a fresh pair, so recovering a gap in one
chat otherwise means deleting `whatsapp.db` and re-syncing everything. To ask
the phone for older messages in a single chat *without* re-pairing:

```bash
curl -X POST http://127.0.0.1:8080/api/history \
  -H "Authorization: Bearer $(cat whatsapp-bridge/store/.bridge-token)" \
  -H "Content-Type: application/json" \
  -d '{"chat_jid": "1234567890@s.whatsapp.net", "count": 50}'
```

The request is anchored on the **oldest message already stored** for that chat,
so the phone returns messages from before it. Call it repeatedly to page
further back. Results arrive asynchronously through the normal history-sync
handler and land in `messages.db` — typically within a few seconds.

| Field | Required | Description |
| --------- | -------- | ------------------------------------------------------ |
| `chat_jid` | yes | Chat to backfill (`...@s.whatsapp.net` or `...@g.us`) |
| `count` | no | Messages to request; default `50`, capped at `500` |

Caveats:

- **The phone decides how much it returns**, exactly as with pair-time sync, so
  `count` is a request rather than a guarantee.
- **At least one message for the chat must already be stored**, since it is used
  as the anchor. Chats with no local messages return `404`; send or receive one
  message first.
- Messages the phone has deleted are not recoverable, as above.

## Call History

The bridge captures incoming WhatsApp voice and video calls live into a
dedicated `calls` table in `messages.db`. When a 1:1 call arrives
(`CallOffer`) or a group call is announced (`CallOfferNotice`), a row is
inserted with `result='in_progress'`. Subsequent `CallAccept` /
`CallReject` / `CallTerminate` events update the row — final result becomes
`answered`, `rejected`, `missed`, or `ended` depending on the event
sequence. See the state-machine comment above `StoreCallOffer` in `main.go`
for the exact transitions.

### Schema

```sql
CREATE TABLE calls (
    call_id TEXT,
    chat_jid TEXT,          -- group JID for group calls, call creator JID for 1:1
    from_jid TEXT,          -- JID of whoever started the call
    timestamp TIMESTAMP,    -- call start time
    is_from_me BOOLEAN,
    call_type TEXT,         -- 'voice' or 'video'
    is_group BOOLEAN,
    result TEXT,            -- 'in_progress' | 'answered' | 'ended' |
                            --   'missed' | 'rejected'
    duration_sec INTEGER,   -- computed when the call terminates
    ended_at TIMESTAMP,
    reason TEXT,            -- terminate reason string from whatsmeow
    PRIMARY KEY (call_id, chat_jid)
);
```

### Caveats

- **Outbound calls are not captured.** WhatsApp's primary device handles
  calls it initiates without notifying linked devices, so the bridge never
  sees an event for them.
- **Call results only reflect what the bridge saw.** If the bridge is
  offline when a call happens, the events are lost.
- **1:1 calls default to `call_type='voice'`.** `CallOffer` events don't
  expose media type directly (it's buried in the binary call data). Group
  calls via `CallOfferNotice` include a `Media` field and are recorded
  accurately as voice or video.

## Architecture

```mermaid
flowchart TB
    subgraph Clients["AI Clients"]
        CD[Claude Desktop]
        CU[Cursor IDE]
        CC[Claude Code]
    end

    subgraph MCP["MCP Layer"]
        PY[Python MCP Server<br/>FastMCP]
    end

    subgraph Bridge["WhatsApp Bridge"]
        GO[Go Bridge<br/>whatsmeow]
        DB[(SQLite<br/>messages.db)]
        WH[Webhook Handler]
    end

    subgraph External["External Services"]
        WA[WhatsApp Web API]
        EXT[External Webhook<br/>Receiver]
    end

    CD & CU & CC -->|MCP Protocol| PY
    PY -->|REST API| GO
    PY -->|Read| DB
    GO -->|Store| DB
    GO <-->|WebSocket| WA
    GO -->|Forward Messages| WH
    WH -->|POST| EXT
```

### Component Details

```mermaid
flowchart LR
    subgraph GoAPI["Go Bridge REST API"]
        direction TB
        SEND["/api/send"]
        READ["/api/mark-read"]
        DOWN["/api/download"]
        REACT["/api/react"]
        TYPE["/api/typing"]
        HIST["/api/history"]
        HEALTH["/api/health"]
    end

    subgraph MCPTools["MCP Tools (15 total)"]
        direction TB
        CONT["Contact Tools<br/>search_contacts, get_contact"]
        MSG["Message Tools<br/>list_messages, send_message, etc."]
        CHAT["Chat Tools<br/>list_chats, get_chat, etc."]
        MEDIA["Media Tools<br/>send_file, download_media, etc."]
    end

    MCPTools -->|HTTP Requests| GoAPI
```

### Data Flow

```mermaid
sequenceDiagram
    participant User as User
    participant Claude as Claude Desktop
    participant MCP as Python MCP Server
    participant Bridge as Go Bridge
    participant WA as WhatsApp

    User->>Claude: "Send 'Hello' to Mom"
    Claude->>MCP: send_message(recipient, message)
    MCP->>Bridge: POST /api/send
    Bridge->>WA: Send via WebSocket
    WA-->>Bridge: Delivery confirmation
    Bridge-->>MCP: Success response
    MCP-->>Claude: Message sent
    Claude-->>User: "Message sent to Mom"
```

### Incoming Message Flow

```mermaid
sequenceDiagram
    participant WA as WhatsApp
    participant Bridge as Go Bridge
    participant DB as SQLite
    participant WH as Webhook
    participant EXT as External Service

    WA->>Bridge: New message
    Bridge->>DB: Store message
    Bridge->>Bridge: Auto-download media
    Bridge->>WH: Forward to webhook
    WH->>EXT: POST with message data
    Note over EXT: Process incoming message
```

## Development

### Running Tests

```bash
cd whatsapp-mcp-server
uv pip install -e ".[dev]"
uv run pytest -v
```

### Linting

```bash
# Python
cd whatsapp-mcp-server
uv run ruff check .
uv run ruff format .

# Go
cd whatsapp-bridge
golangci-lint run
```

### Building

```bash
# Go bridge
cd whatsapp-bridge
go build -o whatsapp-bridge

# Run the binary
./whatsapp-bridge

# During development (avoids stale binaries)
go run .
```

### Releasing (Maintainers)

Releases use Release Please automation; maintainer steps and fallback procedures
are documented in [docs/RELEASING.md](docs/RELEASING.md).

## Troubleshooting

### Authentication Issues

- **Pairing fails with `Client outdated` or HTTP 405**: Update to the latest
  release and rebuild the bridge. WhatsApp periodically raises the minimum
  supported linked-device client version, which can make older whatsmeow builds
  fail before pairing completes.
- **QR Code Not Displaying**: Restart the bridge. Check terminal QR code support.
- **Device Limit Reached**: Remove a linked device from WhatsApp Settings > Linked Devices.
- **No Messages Loading**: Initial sync can take several minutes for large chat histories.
- **Out of Sync**: Back up `whatsapp-bridge/store`, then move
  `whatsapp-bridge/store/whatsapp.db` aside and re-authenticate. Keep
  `messages.db` unless you intentionally want to discard local message history.
- **Bridge returns 401 Unauthorized**: Restart the bridge so it creates
  `.bridge-token` next to `WHATSMEOW_DB_PATH`, then restart the MCP server. If
  the MCP server cannot read that file, set `WHATSAPP_BRIDGE_TOKEN` to the same
  value in both environments.
- **Bridge returns 403 Forbidden for Host**: Use `WHATSAPP_API_URL` with
  `http://127.0.0.1:<port>/api`, `http://localhost:<port>/api`, or
  `http://[::1]:<port>/api`; custom hostnames and missing ports are rejected.
- **Bridge returns 403 Forbidden for media_path**: Move the file into
  `~/.local/share/whatsapp-mcp/outbox` or add its absolute parent directory to
  `WHATSAPP_MEDIA_ROOTS`.

### App State / LTHash Conflicts

Some WhatsApp account state is managed by whatsmeow in
`whatsapp-bridge/store/whatsapp.db`. If the bridge reports errors like:

```text
SendAppState failed: server returned error updating app state (regular_low):
<error code="409" text="conflict"/>
failed to verify patch v12345: mismatching LTHash
```

then WhatsApp's app-state patch chain for the linked device is out of sync.
This usually affects operations that write chat settings such as archive,
mute, or pin state. Incoming and outgoing messages may still work because
message storage lives separately in `messages.db`.

Known manual resync attempts such as `FetchAppState(..., fullSync=true)` may
still fail on this upstream app-state error class. The practical recovery path
is to reset the whatsmeow session and re-pair:

```bash
# Stop the bridge first.
launchctl bootout gui/$UID/com.whatsapp-mcp.bridge    # or however you manage it

# Back up the whole runtime store.
cp -a whatsapp-bridge/store whatsapp-bridge/store.bak.$(date +%Y%m%d%H%M%S)

# Reset only the whatsmeow session/app-state DB.
mv whatsapp-bridge/store/whatsapp.db whatsapp-bridge/store/whatsapp.db.lthash.bak

# Restart the bridge and scan the new QR code.
cd whatsapp-bridge
./whatsapp-bridge    # or `go run .` during development
```

Do not remove `whatsapp-bridge/store/messages.db` for this recovery unless you
also want to delete the local message archive.

### Windows

Windows requires CGO for go-sqlite3. Install [MSYS2](https://www.msys2.org/) and enable CGO:

```bash
go env -w CGO_ENABLED=1
go run .
```

## Security Notice

> **Caution**: As with many MCP servers, this is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Prompt injection could lead to private data exfiltration. Use with awareness.

## License

MIT, see [LICENSE](LICENSE). Original work (c) 2025 Luke Harries; modifications (c) 2026 Very Good
Plugins and, for qx-whatsapp-mcp, (c) 2026 Quinn Frederick. Unofficial; not affiliated with
WhatsApp or Meta.

## Credits & History

This project is a maintained fork of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp), originally created by [Luke Harries](https://github.com/lharries).

**Why we forked:** The original repository hasn't been updated since April 2025. We needed continued maintenance, bug fixes, and new features for production use.

**Highlights since the fork:**

- `/api/typing`, `/api/health`, and webhook forwarding (with reply context + image media)
- Auto-download of incoming media with collision-safe filenames
- `get_contact` tool, `sender_display` field, and LID ↔ phone resolution via the whatsmeow store
- Live capture of incoming voice/video calls into a `calls` table
- `--full-history-pair` flag to request extended history at pair time
- Resilience: recovers from `StreamReplaced` session conflicts; pinned `anyio` to dodge a cancel-scope regression
- CI/CD with GitHub Actions, Release Please for automated versioning, and Dependabot

The full release-by-release list lives in [CHANGELOG.md](CHANGELOG.md).

**Recent contributors** (huge thanks):

- [@edmenendez](https://github.com/edmenendez) — call capture (#39), full-history flag (#37), caption surfacing (#42), media filename collisions (#40), download race fix (#41), LID matching (#43), contact resolution via whatsmeow store (#30)
- [@davidsimoes](https://github.com/davidsimoes) — `StreamReplaced` recovery (#27)
- [@davidggphy](https://github.com/davidggphy) — LID → phone JID consistency (#12)
- [@maikol-solis](https://github.com/maikol-solis) — bridge run command fix (#23)
- [@DeetBot](https://github.com/DeetBot) — `anyio` cancel-scope pin (#44)

And to Luke for creating the original project. See [CONTRIBUTING.md](CONTRIBUTING.md) if you'd like to join in.

## Links

- [Very Good Plugins](https://verygoodplugins.com/?utm_source=github)
- [MCP Specification](https://modelcontextprotocol.io/)
- [whatsmeow](https://github.com/tulir/whatsmeow) - WhatsApp Web API library for Go
- [FastMCP](https://github.com/jlowin/fastmcp) - Fast Model Context Protocol implementation
