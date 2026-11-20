import os
import signal
import sys
from typing import Any

from mcp.server.fastmcp import FastMCP

from mcp_config import resolve_host, resolve_port, resolve_transport
from parent_watchdog import install_stdio_parent_watchdog
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
    mark_messages_read as whatsapp_mark_messages_read,
)
from whatsapp import (
    msg_to_dict,
)
from whatsapp import (
    resolve_contacts as whatsapp_resolve_contacts,
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
             country code, or a group JID ending in @g.us
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
        sub_groups (jid, name, is_announcement_group, joined), announcement group first.
        sub_groups_source is "server" for the full list or "joined_only" when only your own
        groups could be listed; warnings says why for each such community.
    """
    return whatsapp_list_communities(community_jid=community_jid, include_sub_groups=include_sub_groups)


@mcp.tool()
def list_community_participants(community_jid: str, with_names: bool = True) -> dict[str, Any]:
    """List the members of a WhatsApp Community: everyone in any of its groups, once each.

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
