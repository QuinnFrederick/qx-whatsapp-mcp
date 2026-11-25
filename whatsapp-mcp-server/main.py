import os
import signal
import sys
from typing import Any

from mcp.server.fastmcp import FastMCP

from mcp_config import resolve_host, resolve_port, resolve_transport
from parent_watchdog import install_stdio_parent_watchdog
from people import (
    add_person as people_add_person,
)
from people import (
    create_event as people_create_event,
)
from people import (
    event_report as people_event_report,
)
from people import (
    mark_attended as people_mark_attended,
)
from people import (
    query_people as people_query_people,
)
from people import (
    remove_person as people_remove_person,
)
from people import (
    render_review_page as people_render_review_page,
)
from people import (
    start_send_job as people_start_send_job,
)
from people import (
    sync_event_group as people_sync_event_group,
)
from people import (
    update_person as people_update_person,
)
from whatsapp import (
    cancel_send_job as whatsapp_cancel_send_job,
)
from whatsapp import (
    create_community as whatsapp_create_community,
)
from whatsapp import (
    create_group as whatsapp_create_group,
)
from whatsapp import (
    download_media as whatsapp_download_media,
)
from whatsapp import (
    get_chat as whatsapp_get_chat,
)
from whatsapp import (
    get_contact_chats as whatsapp_get_contact_chats,
)
from whatsapp import (
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
)
from whatsapp import (
    get_group_invite_link as whatsapp_get_group_invite_link,
)
from whatsapp import (
    get_last_interaction as whatsapp_get_last_interaction,
)
from whatsapp import (
    get_message_context as whatsapp_get_message_context,
)
from whatsapp import (
    get_profile_picture as whatsapp_get_profile_picture,
)
from whatsapp import (
    get_send_job as whatsapp_get_send_job,
)
from whatsapp import (
    get_sender_name as whatsapp_get_sender_name,
)
from whatsapp import (
    list_chats as whatsapp_list_chats,
)
from whatsapp import (
    list_communities as whatsapp_list_communities,
)
from whatsapp import (
    list_community_participants as whatsapp_list_community_participants,
)
from whatsapp import (
    list_messages as whatsapp_list_messages,
)
from whatsapp import (
    list_send_jobs as whatsapp_list_send_jobs,
)
from whatsapp import (
    mark_messages_read as whatsapp_mark_messages_read,
)
from whatsapp import (
    msg_to_dict,
)
from whatsapp import (
    pause_send_job as whatsapp_pause_send_job,
)
from whatsapp import (
    resolve_contacts as whatsapp_resolve_contacts,
)
from whatsapp import (
    resume_send_job as whatsapp_resume_send_job,
)
from whatsapp import (
    search_contacts as whatsapp_search_contacts,
)
from whatsapp import (
    send_audio_message as whatsapp_audio_voice_message,
)
from whatsapp import (
    send_file as whatsapp_send_file,
)
from whatsapp import (
    send_message as whatsapp_send_message,
)
from whatsapp import (
    send_reaction as whatsapp_send_reaction,
)
from whatsapp import (
    set_group_picture as whatsapp_set_group_picture,
)
from whatsapp import (
    update_community_groups as whatsapp_update_community_groups,
)
from whatsapp import (
    update_group_participants as whatsapp_update_group_participants,
)
from whatsapp import (
    update_group_settings as whatsapp_update_group_settings,
)

# Initialize FastMCP server. Env-var handling is deferred to the __main__ block
# so importing this module never parses env vars or exits the process.
mcp = FastMCP("whatsapp")


@mcp.tool()
def search_contacts(query: str) -> list[dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number.

    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = whatsapp_search_contacts(query)
    return contacts


@mcp.tool()
def get_contact(
    identifier: str | None = None,
    phone_number: str | None = None,
    phone: str | None = None,
) -> dict[str, Any]:
    """Look up a WhatsApp contact by phone number, LID, or full JID.

    Automatically detects the identifier type and queries appropriately.

    Args:
        identifier: Phone number, LID, or full JID. Examples:
                    - "12025551234" (phone number)
                    - "35047067385985" (LID - numeric)
                    - "12025551234@s.whatsapp.net" (phone JID)
                    - "184125298348272@lid" (LID JID)
        phone_number: Backward-compatible alias for `identifier`.
        phone: Backward-compatible alias for `identifier` (matches README parameter name).

    Returns:
        Dictionary with jid, name, display_name, is_lid, and resolved status
    """
    if identifier is None:
        identifier = phone_number
    if identifier is None:
        identifier = phone
    if identifier is None:
        raise ValueError("Missing required argument: identifier (or phone_number / phone)")

    identifier = identifier.strip()
    if not identifier:
        raise ValueError("identifier must be non-empty")

    # Detect identifier type and normalize to JID.
    bare_numeric_digits: str | None = None
    if "@" in identifier:
        # Already a JID - use as-is
        jid = identifier
        is_lid = jid.endswith("@lid") or jid.split("@", 1)[-1] == "lid"
    else:
        digits = "".join(c for c in identifier if c.isdigit())
        if digits:
            # LIDs can overlap phone-number lengths, so bare numeric inputs try phone first.
            jid = f"{digits}@s.whatsapp.net"
            is_lid = False
            if identifier.isdigit():
                bare_numeric_digits = digits
        else:
            # Non-numeric and not a JID; try as-is.
            jid = identifier
            is_lid = False

    jid_user = jid.split("@", 1)[0]

    display_name: str | None = None
    resolved = False

    # Prefer chats table lookup via get_chat (works for both phone and LID contacts).
    candidates: list[tuple[str, bool]] = [(jid, is_lid)]
    if bare_numeric_digits:
        candidates.append((f"{bare_numeric_digits}@lid", True))

    chat = None
    for candidate_jid, candidate_is_lid in candidates:
        chat = whatsapp_get_chat(candidate_jid, include_last_message=False)
        if chat:
            jid = candidate_jid
            is_lid = candidate_is_lid
            jid_user = jid.split("@", 1)[0]
            break

    if chat and chat.get("name"):
        display_name = chat["name"]
        resolved = display_name not in (jid, jid_user)
    else:
        # Fallback: best-effort sender-name resolution (may use fuzzy LIKE lookup).
        display_name = whatsapp_get_sender_name(jid)
        resolved = display_name not in (jid, jid_user, identifier)

    return {
        "identifier": identifier,
        "jid": jid,
        "phone_number": jid_user if not is_lid else None,
        "lid": jid_user if is_lid else None,
        "name": display_name if resolved else jid_user,
        "display_name": display_name,
        "is_lid": is_lid,
        "resolved": resolved,
    }


@mcp.tool()
def list_messages(
    after: str | None = None,
    before: str | None = None,
    sender_phone_number: str | None = None,
    chat_jid: str | None = None,
    query: str | None = None,
    limit: int = 50,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1,
    sort_by: str = "newest",
) -> list[dict[str, Any]]:
    """Get WhatsApp messages matching specified criteria with optional context.

    Each message includes sender_display showing "Name (phone)" for easy identification.

    Args:
        after: ISO-8601 date string (e.g., "2026-01-01" or "2026-01-01T09:00:00")
        before: ISO-8601 date string (e.g., "2026-01-09" or "2026-01-09T18:00:00")
        sender_phone_number: Phone number to filter by sender (e.g., "12025551234")
        chat_jid: Chat JID to filter by (e.g., "12025551234@s.whatsapp.net" or group JID)
        query: Search term to filter messages by content
        limit: Max messages to return (default 50, max 500)
        page: Page number for pagination (default 0)
        include_context: Include surrounding messages for context (default True)
        context_before: Messages to include before each match (default 1)
        context_after: Messages to include after each match (default 1)
        sort_by: "newest" (default, most recent first) or "oldest" (chronological)
    """
    # Cap limit at 500 to prevent excessive queries
    limit = min(limit, 500)
    messages = whatsapp_list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after,
        sort_by=sort_by,
    )
    return messages


@mcp.tool()
def list_chats(
    query: str | None = None,
    limit: int = 50,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
) -> list[dict[str, Any]]:
    """Get WhatsApp chats matching specified criteria.

    Args:
        query: Search term to filter chats by name or JID
        limit: Max chats to return (default 50, max 200)
        page: Page number for pagination (default 0)
        include_last_message: Include the last message in each chat (default True)
        sort_by: "last_active" (default, most recent first) or "name" (alphabetical)

    Returns:
        Chat dictionaries with jid, name, is_group, last_message_time, last_message,
        last_sender, last_is_from_me, last_read_time and unread. `last_read_time` is
        how far the chat has been read on any device (null if never reported); `unread`
        is true when the last message is inbound and newer than that marker, so chats
        already read on the phone are not reported as unread.
    """
    # Cap limit at 200 to prevent excessive queries
    limit = min(limit, 200)
    chats = whatsapp_list_chats(
        query=query, limit=limit, page=page, include_last_message=include_last_message, sort_by=sort_by
    )
    return chats


@mcp.tool()
def get_chat(chat_jid: str, include_last_message: bool = True) -> dict[str, Any]:
    """Get WhatsApp chat metadata by JID.

    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)

    Returns:
        Chat dictionary — same shape as list_chats, including last_read_time and unread.
    """
    chat = whatsapp_get_chat(chat_jid, include_last_message)
    return chat


@mcp.tool()
def get_direct_chat_by_contact(sender_phone_number: str) -> dict[str, Any]:
    """Get WhatsApp chat metadata by sender phone number.

    Args:
        sender_phone_number: The phone number to search for
    """
    chat = whatsapp_get_direct_chat_by_contact(sender_phone_number)
    return chat


@mcp.tool()
def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> list[dict[str, Any]]:
    """Get all WhatsApp chats involving the contact.

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    chats = whatsapp_get_contact_chats(jid, limit, page)
    return chats


@mcp.tool()
def get_last_interaction(jid: str) -> dict[str, Any]:
    """Get most recent WhatsApp message involving the contact.

    Args:
        jid: The JID of the contact to search for

    Returns:
        Message dictionary with id, timestamp, sender, content, etc. or empty dict if not found.
    """
    message = whatsapp_get_last_interaction(jid)
    return message if message else {}


@mcp.tool()
def get_message_context(message_id: str, before: int = 5, after: int = 5) -> dict[str, Any]:
    """Get context around a specific WhatsApp message.

    Args:
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    context = whatsapp_get_message_context(message_id, before, after)
    return {
        "message": msg_to_dict(context.message),
        "before": [msg_to_dict(message) for message in context.before],
        "after": [msg_to_dict(message) for message in context.after],
    }


@mcp.tool()
def send_message(
    recipient: str,
    message: str,
    quoted_message_id: str = "",
    quoted_sender_jid: str = "",
    quoted_content: str = "",
    mentions: list[str] | None = None,
) -> dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        message: The message text to send
        quoted_message_id: ID of the message to reply to (optional). When set, the sent
                           message will appear as a quoted reply in WhatsApp.
        quoted_sender_jid: Full JID of the author of the quoted message. Required for
                           group replies so WhatsApp renders the correct attribution.
        quoted_content: Text content of the quoted message, used for the reply preview.
                        Only plain text is supported; media previews are not included.
        mentions: Users to @-mention, as phone numbers with country code but no + (e.g.
                  ["420601234567"]) or JIDs. For each entry the message text must contain
                  a matching "@<number>" token (e.g. "hi @420601234567"), otherwise the
                  mention won't render on recipients' devices. Only meaningful in groups.

    Returns:
        A dictionary containing success status and a status message
    """
    # Validate input
    if not recipient:
        return {"success": False, "message": "Recipient must be provided"}

    # Call the whatsapp_send_message function with the unified recipient parameter
    success, status_message = whatsapp_send_message(
        recipient, message, quoted_message_id, quoted_sender_jid, quoted_content, mentions
    )
    return {"success": success, "message": status_message}


@mcp.tool()
def send_reaction(
    recipient: str,
    message_id: str,
    emoji: str,
    from_me: bool = False,
    sender_jid: str = "",
) -> dict[str, Any]:
    """Send (or remove) a reaction to a WhatsApp message.

    Args:
        recipient: The chat JID the message belongs to (e.g., "12025551234@s.whatsapp.net"
                   or a group JID like "123456789@g.us")
        message_id: The ID of the message to react to
        emoji: The reaction emoji (e.g., "👍"). Pass an empty string to remove the reaction.
        from_me: Whether the original message was sent by the current user (default False)
        sender_jid: JID of the original message sender — required for group messages when
                    from_me is False so the bridge can build the correct WhatsApp key

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_send_reaction(recipient, message_id, emoji, from_me, sender_jid)
    return {"success": success, "message": status_message}


@mcp.tool()
def mark_messages_read(
    message_ids: list[str],
    chat_jid: str,
    sender_jid: str = "",
    timestamp: str | None = None,
) -> dict[str, Any]:
    """Mark selected WhatsApp messages as read and send read receipts.

    This is an explicit external side effect. All message IDs must belong to the
    same chat and sender.

    Args:
        message_ids: IDs of the messages to mark as read
        chat_jid: JID of the chat containing the messages
        sender_jid: JID or bare phone number of the original sender; required for groups
        timestamp: Optional RFC 3339 read timestamp; defaults to the current time

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_mark_messages_read(message_ids, chat_jid, sender_jid, timestamp)
    return {"success": success, "message": status_message}


@mcp.tool()
def send_file(recipient: str, media_path: str, caption: str = "") -> dict[str, Any]:
    """Send a file (image, video, document) via WhatsApp, optionally with a caption.

    When `caption` is provided, the file and text arrive as a single
    attachment-with-caption message (one bubble in the WA UI), instead of
    needing a separate follow-up send_message call. For group chats use the JID.

    Args:
        recipient: Either a phone number with country code (no + or symbols),
                 or a JID (e.g., "123456789@s.whatsapp.net" or "123456789@g.us")
        media_path: Absolute path to the media file (image, video, document)
        caption: Optional text rendered with the file as a caption. Omit for a
                 bare attachment.

    Returns:
        A dictionary containing success status and a status message
    """

    # Call the whatsapp_send_file function
    success, status_message = whatsapp_send_file(recipient, media_path, caption)
    return {"success": success, "message": status_message}


@mcp.tool()
def send_audio_message(recipient: str, media_path: str) -> dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_audio_voice_message(recipient, media_path)
    return {"success": success, "message": status_message}


@mcp.tool()
def download_media(message_id: str, chat_jid: str) -> dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        A dictionary containing success status, a status message, and the file path if successful
    """
    file_path = whatsapp_download_media(message_id, chat_jid)

    if file_path:
        return {"success": True, "message": "Media downloaded successfully", "file_path": file_path}
    else:
        return {"success": False, "message": "Failed to download media"}


@mcp.tool()
def resolve_contacts(names: list[str], max_matches: int = 5, with_avatars: bool = True) -> list[dict[str, Any]]:
    """Turn a list of spoken or typed names into WhatsApp contacts, flagging which names are ambiguous.

    Use this before create_group or update_group_participants whenever the user names people
    instead of giving phone numbers. Each name is searched the same way as search_contacts.

    For every name you get back:
      - status: "unique" (exactly one contact matched, so its jid is safe to use),
        "ambiguous" (several contacts matched; ask the user which one they mean and show each
        match's name, phone and avatar_url so they can pick), or
        "none" (nothing matched; ask for a phone number or a different spelling).
      - matches: up to max_matches contacts, best match first, each with jid, name, phone and
        avatar_url. avatar_url is a short-lived WhatsApp CDN link to a small profile picture.
        It is only fetched for ambiguous names and is null when the person has no picture,
        hides it, or the bridge is unavailable.
      - total_matches: how many contacts matched before the max_matches cap.

    Args:
        names: Names to resolve, e.g. ["Harry", "Sam Jones", "Mum"]. Phone numbers also work.
        max_matches: Cap on matches returned per name (default 5, max 20)
        with_avatars: Fetch profile pictures for ambiguous names (default True)
    """
    return whatsapp_resolve_contacts(names, max_matches=max_matches, with_avatars=with_avatars)


@mcp.tool()
def get_profile_picture(jid: str, preview: bool = True) -> dict[str, Any]:
    """Get the URL of a contact's or group's WhatsApp profile picture.

    Args:
        jid: Contact JID ("12025551234@s.whatsapp.net" or "...@lid"), a phone number with
             country code, or a group or community JID ending in @g.us
        preview: True (default) returns a small thumbnail; False returns the full-size image

    Returns:
        success, jid and url. url is null when there is no picture, the person hides it from
        you, or the bridge is offline (reason/message say which). URLs are short-lived
        WhatsApp CDN links, so use them promptly rather than storing them.
    """
    return whatsapp_get_profile_picture(jid, preview=preview)


@mcp.tool()
def create_group(
    name: str,
    participants: list[str],
    description: str | None = None,
    announce: bool | None = None,
    locked: bool | None = None,
    member_add: str | None = None,
    join_approval: bool | None = None,
    community_jid: str | None = None,
) -> dict[str, Any]:
    """Create a new WhatsApp group and add people to it, optionally inside a community.

    Resolve names to JIDs first with resolve_contacts and pass the chosen jids here. Your own
    account is added automatically. People whose privacy settings block being added are
    listed in failed_participants with an invite code, and the group is still created.

    Args:
        name: Group subject, up to 100 characters
        participants: People to add, as JIDs ("12025551234@s.whatsapp.net", "...@lid") or
                      phone numbers with country code. At least one is required.
        description: Optional text shown in the group's info screen
        announce: True makes an "announcement group" where only admins can post messages;
                  False lets everyone post. Omit to keep WhatsApp's default (everyone posts).
        locked: True means only admins can edit the group name, picture and description;
                False lets all members edit them. Omit for WhatsApp's default (all members).
        member_add: Who may add new members: "all_members" or "admins_only". Omit for default.
        join_approval: True means an admin must approve anyone joining via invite link;
                       False lets them join straight away. Omit for default (no approval).
        community_jid: Create the group inside this community (its jid from list_communities)
                       instead of as a standalone group. You must be an admin of the community.
                       To move an existing group into a community use update_community_groups.

    Returns:
        success, group (jid, name, description, announce, locked, member_add, join_approval,
        community_jid, participants), plus warnings for any setting that could not be applied
        and failed_participants for anyone WhatsApp refused to add.
    """
    return whatsapp_create_group(
        name,
        participants,
        description=description,
        announce=announce,
        locked=locked,
        member_add=member_add,
        join_approval=join_approval,
        community_jid=community_jid,
    )


@mcp.tool()
def update_group_settings(
    group_jid: str,
    name: str | None = None,
    description: str | None = None,
    announce: bool | None = None,
    locked: bool | None = None,
    member_add: str | None = None,
    join_approval: bool | None = None,
) -> dict[str, Any]:
    """Change a WhatsApp group's name, description or admin settings. Pass only the fields to change.

    You must be a group admin. Calling this with just group_jid changes nothing and returns
    the group's current settings and participant list.

    Args:
        group_jid: The group's JID, ending in @g.us
        name: New group subject, up to 100 characters
        description: New description. Pass an empty string to clear it.
        announce: True means only admins can post ("announcement group"); False lets everyone post
        locked: True means only admins can edit the group name, picture and description
        member_add: Who may add new members: "all_members" or "admins_only"
        join_approval: True means an admin must approve people joining via invite link

    Returns:
        success (False if any requested setting failed; warnings says which) and group with
        the current state after the change.
    """
    return whatsapp_update_group_settings(
        group_jid,
        name=name,
        description=description,
        announce=announce,
        locked=locked,
        member_add=member_add,
        join_approval=join_approval,
    )


@mcp.tool()
def update_group_participants(group_jid: str, participants: list[str], action: str) -> dict[str, Any]:
    """Add or remove people in a WhatsApp group, or make them admins (promote) or regular members (demote).

    You must be a group admin. Each person gets their own ok flag in results, so one failure
    (for example privacy settings that block being added) does not stop the others.

    Args:
        group_jid: The group's JID, ending in @g.us
        participants: JIDs ("12025551234@s.whatsapp.net", "...@lid") or phone numbers with country code
        action: "add", "remove", "promote" or "demote"

    Returns:
        success (True only if every participant succeeded), action, and results with jid, ok,
        and for failures an error code, error_hint and sometimes an add_request invite code.
    """
    return whatsapp_update_group_participants(group_jid, participants, action)


@mcp.tool()
def get_group_invite_link(group_jid: str, reset: bool = False) -> dict[str, Any]:
    """Get a WhatsApp group's invite link (https://chat.whatsapp.com/...), or reset it.

    Only group admins can see or reset the link. A community's jid returns the community's
    own invite link.

    Args:
        group_jid: The group's (or community's) JID, ending in @g.us
        reset: False (default) only reads the current link. True REVOKES the current link,
               so nobody can join with it any more, and returns a new one. Only reset when
               the user explicitly asks for a new link.

    Returns:
        success, group_jid, link, code (the part after chat.whatsapp.com/) and reset. On
        failure, message says why (for example that only admins can see the link).
    """
    return whatsapp_get_group_invite_link(group_jid, reset=reset)


@mcp.tool()
def set_group_picture(
    group_jid: str,
    image_url: str | None = None,
    image_path: str | None = None,
    image_base64: str | None = None,
    remove: bool = False,
) -> dict[str, Any]:
    """Set or remove the picture of a WhatsApp group, or of a community itself.

    Give exactly ONE image source: image_url, image_path or image_base64. Or pass
    remove=True with no image to clear the current picture. Only change a picture when the
    user asks you to.

    Any JPEG, PNG or WebP up to 10 MB works as it is: it is turned upright, centre-cropped
    to a square, resized to 640x640 and converted to JPEG automatically, so do not crop,
    resize or convert it first. A non-square image loses its edges on the long side.

    For a community's own picture, pass the community's jid from list_communities. Each
    group inside a community has its own picture; pass that group's jid to change it.

    Who may do this: community admins for a community's picture. For a group, its admins;
    if the group lets all members edit its info (locked=False in update_group_settings),
    any member.

    Args:
        group_jid: The group's or community's JID, ending in @g.us
        image_url: A public https:// link straight to a JPEG, PNG or WebP image (not a web
            page). The bridge downloads it; links to local or private network addresses
            are refused.
        image_path: Absolute path to an image file on the computer running the WhatsApp
            bridge, inside its allowed media folders (WHATSAPP_MEDIA_ROOTS, by default
            ~/.local/share/whatsapp-mcp/outbox)
        image_base64: The image file's bytes, base64-encoded. A data: URL
            ("data:image/png;base64,...") is fine too.
        remove: True removes the current picture. Give no image source with it.

    Returns:
        success, group_jid, is_community (null if it could not be checked), removed, and
        picture_id (WhatsApp's id for the new picture; null after a removal). When an image
        was set, image gives source, format, original_width, original_height, width,
        height and bytes. On failure, message says why (for example that only admins can
        change the picture, or that the image could not be downloaded).
    """
    return whatsapp_set_group_picture(
        group_jid,
        image_url=image_url,
        image_path=image_path,
        image_base64=image_base64,
        remove=remove,
    )


@mcp.tool()
def list_communities(community_jid: str | None = None, include_sub_groups: bool | None = None) -> dict[str, Any]:
    """List the WhatsApp Communities you belong to, with the groups inside each one.

    A community is a parent that holds several groups. Its announcement group is created
    with the community: every community member is in it and only admins can post there.
    Use the community jid from here with create_group(community_jid=...),
    update_community_groups, list_community_participants and get_group_invite_link.

    Without community_jid you get a quick overview: every community, with the groups in it
    that you have joined. Pass one community_jid to see that community's complete group
    list, including groups you have not joined.

    Args:
        community_jid: Optional community JID (ending in @g.us) to show just that community,
            with its complete group list
        include_sub_groups: Leave unset normally. True fetches the complete group list for
            every community, but WhatsApp rate-limits that after about nine communities and
            the rest then show only your own groups; False skips it even for one community.

    Returns:
        success and communities, each with jid, name, description, you_are_admin (true/false,
        or null if unknown), announcement_group (jid, name and joined, or null) and
        sub_groups (jid, name, is_announcement_group, joined, hidden, is_general_chat, size),
        announcement group first. hidden=true marks a group the admins hid from the
        community's group list (only invited members see it). sub_groups_source is "server"
        for the complete list or "joined_only" when only your own groups are shown; there
        hidden and is_general_chat are null (unknown). warnings explains any community whose
        complete list could not be fetched.

    Hiding a group can only be chosen in the WhatsApp app, when the group is created inside
    the community; these tools can read the flag but not set it.
    """
    return whatsapp_list_communities(community_jid=community_jid, include_sub_groups=include_sub_groups)


@mcp.tool()
def list_community_participants(community_jid: str, with_names: bool = True) -> dict[str, Any]:
    """List the members of a WhatsApp Community: everyone in any of its groups, once each.

    Only community admins can do this; WhatsApp refuses it for ordinary members (you_are_admin
    in list_communities tells you which communities qualify).

    Args:
        community_jid: The community's JID from list_communities, ending in @g.us
        with_names: True (default) adds each member's contact name from your address book or
            WhatsApp profile when known (null otherwise)

    Returns:
        success, community_jid, count and participants, each with jid, phone (null when
        WhatsApp only shares an anonymous @lid id and it cannot be mapped), lid and name.
    """
    return whatsapp_list_community_participants(community_jid, with_names=with_names)


@mcp.tool()
def create_community(name: str, description: str | None = None) -> dict[str, Any]:
    """Create a new WhatsApp Community, with you as its admin.

    WhatsApp creates the community's announcement group automatically (every member is in
    it and only admins post there). A new community has no other groups and no members
    yet: add groups with create_group(community_jid=...) or update_community_groups(action=
    "link"), and invite people with get_group_invite_link on the community's jid.

    Args:
        name: Community name, up to 100 characters
        description: Optional text shown on the community's info screen

    Returns:
        success, community (jid, name, description, is_community, ...), announcement_group
        (jid and name, or null if it could not be listed yet) and warnings for any follow-up
        step that failed after the community was created.
    """
    return whatsapp_create_community(name, description=description)


@mcp.tool()
def update_community_groups(community_jid: str, group_jids: list[str], action: str) -> dict[str, Any]:
    """Move existing WhatsApp groups into a community ("link") or take them out ("unlink").

    You must be an admin of the community and of each group. Unlinked groups keep their
    members and messages and become standalone groups again. The community's announcement
    group cannot be unlinked. To make a brand-new group inside a community, use
    create_group(community_jid=...) instead.

    Args:
        community_jid: The community's JID from list_communities, ending in @g.us
        group_jids: Group JIDs (ending in @g.us) to link or unlink
        action: "link" or "unlink"

    Returns:
        success (True only if every group succeeded), action, community_jid and results with
        jid, ok and, for failures, the error from WhatsApp.
    """
    return whatsapp_update_community_groups(community_jid, group_jids, action)


# --- Slow-send jobs (fork addition, see FORK.md) ------------------------------


@mcp.tool()
def start_send_job(
    message_template: str,
    recipients: list[dict[str, str] | str] | None = None,
    people_filter: dict[str, Any] | None = None,
    event_id: int | None = None,
    label: str | None = None,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Send one personal WhatsApp message to each of many people, slowly, in the background.

    Use this, not send_message in a loop, for invites or announcements to more than a few
    people. The bridge sends one message at a time with a random 45-90 second gap and a
    5 minute break after every 25 sends (the bridge's WHATSAPP_SEND_JOB_* settings), so 60
    people take about 75 minutes. The job keeps running after this conversation ends and
    survives bridge restarts; follow it with get_send_job.

    Choose who gets it with recipients, people_filter, or both (they are merged):
    - recipients: explicit people (resolve names to JIDs with resolve_contacts first).
    - people_filter: people from the private people list, chosen exactly like query_people,
      e.g. {"min_flavour": "warm", "places": ["Stroud"], "tags_exclude": ["birthdays-only"]},
      or everyone who attended an earlier event: {"event_id": 3, "event_statuses":
      ["attended"]}. Their display_name fills {name}. Run query_people with the same filter
      first and show the user who it selects.
    - event_id: the event these invites are for. Everyone the job sends to is recorded as
      "invited" on that event (see event_report). With event_id every recipient must be on
      the people list. Dry runs are not recorded.

    Before calling, get the user's approval of the final recipient list and the exact
    message: sent messages cannot be recalled. Use dry_run=True to rehearse.

    Rules the bridge enforces:
    - Each person is messaged at most once per job. The same person given twice (by phone
      and by JID, or with different formatting) is dropped and listed in duplicates_removed.
    - At most 100 unique recipients per job; a bigger list is rejected, never truncated.
    - Anyone already in another unfinished (running or paused) job is refused: resume that
      job rather than starting a new one, or the same people get the message twice.
    - If a send fails the job pauses itself; read pause_reason, fix the cause, then call
      resume_send_job.
    - Jobs run one at a time. A new job waits with status "queued" behind the running one.

    Args:
        message_template: The message text. Every "{name}" becomes that recipient's name, and
                    then every recipient needs a name. No other {placeholders} exist, so write
                    dates, places and links out in full.
        recipients: Explicit people, each {"recipient": <JID such as
                    "447700900123@s.whatsapp.net" or "...@lid", or a phone number with
                    country code>, "name": <the name to put in {name}>}. Plain JID/phone
                    strings are fine when the template has no {name}. People only: group
                    JIDs are rejected.
        people_filter: A query_people filter (keys: min_flavour, places, tags_include,
                    tags_any, tags_exclude, event_id, event_statuses) picking people from
                    the people list.
        event_id: Event to record the invites on (from create_event or event_report). The
                  job's label defaults to the event's name.
        label: Optional short label to recognise the job later, e.g. "Birthday invites".
        dry_run: True runs the whole job with real pacing and progress tracking but sends
                 nothing to WhatsApp (recipients end up "sent" in a job marked dry_run).

    Returns:
        success and message, job (id, status, counts, next_send_at,
        estimated_remaining_seconds, recipients, example_message), duplicates_removed,
        people_selected (with people_filter) and event_note (with event_id). On failure
        nothing was created and message (plus errors, one per bad entry) says what to fix.
    """
    return people_start_send_job(
        message_template,
        recipients=recipients,
        people_filter=people_filter,
        event_id=event_id,
        label=label,
        dry_run=dry_run,
    )


@mcp.tool()
def get_send_job(job_id: int, recipient_status: str | None = None) -> dict[str, Any]:
    """Show a slow-send job's progress: its status, counts and each recipient's status.

    Job status: "running" (sending, or waiting for the next gap), "queued" (waiting for an
    older job to finish), "paused" (nothing is sent until resume_send_job; pause_reason says
    why), "completed" (nothing left to send; check counts.failed) or "cancelled".

    Recipient status: "pending" (not sent yet), "sending" (going out right now), "sent"
    (sent_at says when) or "failed" (error says why; failed recipients are only retried by
    resume_send_job with retry_failed=True).

    For the running job, waiting_for says what it is waiting on: "pacing_delay",
    "batch_break", "whatsapp_connection" (the bridge is offline; the job carries on by itself
    once reconnected), "sending_now" or "ready". next_send_at and estimated_remaining_seconds
    give the timing.

    Args:
        job_id: The job's id, from start_send_job or list_send_jobs
        recipient_status: Only list recipients with this status: "pending", "sending",
                          "sent" or "failed"

    Returns:
        success and job, including recipients (position, recipient as given, jid, name,
        status, attempts, last_attempt_at, sent_at, error), example_message and pacing.
    """
    return whatsapp_get_send_job(job_id, recipient_status=recipient_status)


@mcp.tool()
def list_send_jobs(status: str | None = None, limit: int = 20) -> dict[str, Any]:
    """List slow-send jobs, newest first, with their status and counts.

    Per-recipient detail is left out; use get_send_job for that. Also returns the bridge's
    current pacing settings.

    Args:
        status: Only jobs with this status: "running" (includes queued jobs), "paused",
                "completed" or "cancelled"
        limit: How many jobs to return, 1-200 (default 20)
    """
    return whatsapp_list_send_jobs(status=status, limit=limit)


@mcp.tool()
def pause_send_job(job_id: int) -> dict[str, Any]:
    """Pause a running or queued slow-send job until resume_send_job is called.

    Nothing more is sent while it is paused. A message that is already going out at that
    moment still completes. Pausing an already paused job changes nothing.

    Args:
        job_id: The job's id
    """
    return whatsapp_pause_send_job(job_id)


@mcp.tool()
def resume_send_job(job_id: int, retry_failed: bool = False) -> dict[str, Any]:
    """Resume a paused slow-send job, continuing with the recipients still pending.

    Recipients already sent are never sent again. Pacing picks up where it was, so the next
    message may still wait out the current gap. Failed recipients stay failed unless
    retry_failed is True.

    Before retrying, look at why they failed (get_send_job with recipient_status="failed").
    A failure does not always mean the message was not delivered: if the bridge restarted
    mid-send the error says so, and that chat should be checked first.

    Args:
        job_id: The job's id
        retry_failed: Also try the failed recipients again, once each. Works on a completed
                      job that has failed recipients too.
    """
    return whatsapp_resume_send_job(job_id, retry_failed=retry_failed)


@mcp.tool()
def cancel_send_job(job_id: int) -> dict[str, Any]:
    """Cancel a slow-send job for good.

    Its pending recipients will never be sent by this job, and a cancelled job cannot be
    resumed: use pause_send_job if you might want to continue. A message already going out
    at that moment still completes. If you later start a new job for the same people, leave
    out those this job already sent to, or they will get the message twice.

    Args:
        job_id: The job's id
    """
    return whatsapp_cancel_send_job(job_id)


# --- People list for event invites (fork addition, see FORK.md) -------------------


@mcp.tool()
def add_person(
    query: str | None = None,
    jid: str | None = None,
    flavour: str | None = None,
    display_name: str | None = None,
    place: str | None = None,
    tags: list[str] | None = None,
) -> dict[str, Any]:
    """Add someone to the private people list used for event invites. Two steps.

    1. Call with query (a name or a phone number with country code). Nothing is saved: you
       get candidates (jid, name, phone, avatar_url, already_on_list). Show them to the
       user, with the pictures, and ask which one is meant.
    2. Call again with the confirmed jid and a flavour (ask the user; do not guess). The jid
       must be a contact or chat this WhatsApp account already knows.

    The list is private, encrypted on disk and minimal: only jid, display_name, flavour,
    place and tags are stored. Do not try to store anything else about people (there is no
    notes field).

    Args:
        query: Name or phone number to look up candidates (step 1)
        jid: The confirmed person's JID from the candidates (step 2)
        flavour: How close they are, an ordered scale: "cool" < "warm" < "hot" (step 2)
        display_name: Name to show and to use for {name} in invites; defaults to the contact
                      name. A first name usually reads best in a personal invite.
        place: Where they live, free text such as "Stroud" or "Bristol"
        tags: Free-form labels such as "climbing" or "birthdays-only" (lowercased; spaces
              become hyphens)
    """
    return people_add_person(query=query, jid=jid, flavour=flavour, display_name=display_name, place=place, tags=tags)


@mcp.tool()
def update_person(
    jid: str,
    flavour: str | None = None,
    place: str | None = None,
    add_tags: list[str] | None = None,
    remove_tags: list[str] | None = None,
    display_name: str | None = None,
) -> dict[str, Any]:
    """Change someone on the people list. Only the fields you pass change.

    Args:
        jid: The person's JID (or phone number with country code)
        flavour: "cool", "warm" or "hot"
        place: New place; an empty string clears it
        add_tags: Tags to add
        remove_tags: Tags to remove
        display_name: New display name
    """
    return people_update_person(
        jid, flavour=flavour, place=place, add_tags=add_tags, remove_tags=remove_tags, display_name=display_name
    )


@mcp.tool()
def remove_person(jid: str) -> dict[str, Any]:
    """Remove someone from the people list for good, with their status on every event.

    Their WhatsApp contact and chats are not touched. Messages already sent stay sent.

    Args:
        jid: The person's JID (or phone number with country code)
    """
    return people_remove_person(jid)


@mcp.tool()
def query_people(
    min_flavour: str | None = None,
    places: list[str] | None = None,
    tags_include: list[str] | None = None,
    tags_any: list[str] | None = None,
    tags_exclude: list[str] | None = None,
    event_id: int | None = None,
    event_statuses: list[str] | None = None,
) -> dict[str, Any]:
    """Find people on the private people list. All filters combine (AND); omit one to ignore it.

    The same filter can be passed as people_filter to start_send_job and to
    render_review_page, so check who a filter selects here before sending.

    Args:
        min_flavour: "cool" (everyone), "warm" (warm and hot) or "hot" (hot only)
        places: Only people whose place is one of these (case-insensitive exact match),
                e.g. ["Stroud", "Bristol"]
        tags_include: Only people who have every one of these tags
        tags_any: Only people who have at least one of these tags
        tags_exclude: Leave out anyone with any of these tags, e.g. ["birthdays-only"]
        event_id: Only people with a status on this event (combine with event_statuses)
        event_statuses: With event_id, which statuses count: "invited", "joined",
                        "attended", "declined", or "none" for people not on the event at all
                        (e.g. warm people not yet invited)

    Returns:
        success, count and people (jid, phone, display_name, flavour, place, tags, and
        event_status when event_id is given), sorted by name.
    """
    return people_query_people(
        min_flavour=min_flavour,
        places=places,
        tags_include=tags_include,
        tags_any=tags_any,
        tags_exclude=tags_exclude,
        event_id=event_id,
        event_statuses=event_statuses,
    )


@mcp.tool()
def create_event(name: str, date: str, group_jid: str | None = None) -> dict[str, Any]:
    """Create an event on the people list, to track who was invited, joined and attended.

    Args:
        name: Event name, e.g. "Winter party"
        date: Date as YYYY-MM-DD
        group_jid: Optional WhatsApp group for the event (ending in @g.us). People on the list
                   who are in that group count as "joined"; it can also be linked later with
                   sync_event_group.
    """
    return people_create_event(name, date, group_jid=group_jid)


@mcp.tool()
def sync_event_group(event_id: int, group_jid: str | None = None) -> dict[str, Any]:
    """Mark people as "joined" from the membership of the event's WhatsApp group.

    Reads the group's current participants: people on the list who are in it become
    "joined", and anyone who has left loses that status. Group members who are not on the
    list are only counted (not stored). event_report runs this by itself when the last sync
    is more than 10 minutes old.

    Args:
        event_id: The event
        group_jid: Link (or re-link) this group to the event first, ending in @g.us
    """
    return people_sync_event_group(event_id, group_jid=group_jid)


@mcp.tool()
def mark_attended(event_id: int, people: list[str], status: str = "attended") -> dict[str, Any]:
    """Mark people by hand as having attended (or declined) an event.

    Hand marks win over what was observed: someone marked attended or declined keeps that
    status whatever the group shows. Marking attended clears declined, and the reverse.

    Args:
        event_id: The event
        people: JIDs or phone numbers of people on the list
        status: "attended" (default) or "declined"
    """
    return people_mark_attended(event_id, people, status=status)


@mcp.tool()
def event_report(event_id: int | None = None) -> dict[str, Any]:
    """Report on an event: who was invited, joined, attended or declined. Without event_id,
    lists all events with their counts.

    Each person has one status, the strongest that applies: attended > declined > joined >
    invited. Before reporting, invites are brought up to date from the event's send jobs and
    the event group is re-read if its last sync is over 10 minutes old.

    Args:
        event_id: The event; omit to list events

    Returns:
        success, event, counts per status, ever_invited, people grouped by status (with the
        time of each step), send_jobs (their status and counts) and any warnings.
    """
    return people_event_report(event_id)


@mcp.tool()
def render_review_page(
    min_flavour: str | None = None,
    places: list[str] | None = None,
    tags_include: list[str] | None = None,
    tags_any: list[str] | None = None,
    tags_exclude: list[str] | None = None,
    event_id: int | None = None,
    event_statuses: list[str] | None = None,
    title: str | None = None,
    with_avatars: bool = True,
) -> dict[str, Any]:
    """Write a local HTML contact sheet of people on the list for the user to review.

    Each card shows the profile picture (downloaded into the page), name, number, flavour,
    place and tags. Filters work exactly like query_people. The file is written to a private,
    gitignored folder on this machine and its path returned; tell the user the path rather
    than pasting the page. It holds other people's details, so suggest deleting it when done.

    Args:
        min_flavour, places, tags_include, tags_any, tags_exclude, event_id, event_statuses:
            As in query_people
        title: Heading for the page
        with_avatars: Fetch profile pictures (one WhatsApp lookup per person, paced)

    Returns:
        success, path, count and avatars_included.
    """
    return people_render_review_page(
        min_flavour=min_flavour,
        places=places,
        tags_include=tags_include,
        tags_any=tags_any,
        tags_exclude=tags_exclude,
        event_id=event_id,
        event_statuses=event_statuses,
        title=title,
        with_avatars=with_avatars,
    )


def shutdown_handler(signum, frame):
    """Handle shutdown signals gracefully to prevent zombie processes."""
    sys.exit(0)


if __name__ == "__main__":
    # Capture before any await — os.getppid() is dynamic.
    parent_pid = os.getppid()
    # Register signal handlers for clean shutdown
    signal.signal(signal.SIGINT, shutdown_handler)
    signal.signal(signal.SIGTERM, shutdown_handler)

    # Resolve the transport first: host/port are only used (and validated) for the
    # network transports, so a bad WHATSAPP_MCP_PORT can't break a stdio launch.
    # The localhost default keeps a remote server unreachable until explicitly opened up.
    try:
        transport = resolve_transport(os.getenv("WHATSAPP_MCP_TRANSPORT"))
        if transport != "stdio":
            mcp.settings.host = resolve_host(os.getenv("WHATSAPP_MCP_HOST"))
            mcp.settings.port = resolve_port(os.getenv("WHATSAPP_MCP_PORT"))
            # stdout is reserved for the protocol on stdio; log startup to stderr.
            print(
                f"WhatsApp MCP server listening on {mcp.settings.host}:{mcp.settings.port} via {transport}",
                file=sys.stderr,
            )
    except ValueError as exc:
        raise SystemExit(str(exc)) from None

    if transport == "stdio":
        install_stdio_parent_watchdog("WHATSAPP_PARENT_WATCHDOG_S", parent_pid=parent_pid)
    mcp.run(transport=transport)
