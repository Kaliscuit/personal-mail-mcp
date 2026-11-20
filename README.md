# Personal Mail MCP

English · [简体中文](README.zh-CN.md)

A self-hosted, multi-account mail server for MCP. Trusted agents connect to the Streamable HTTP `/mcp` endpoint and use account aliases to search, organize, and send email. Agents do not receive mailbox credentials or Outlook refresh tokens. No database, background sync, or Tailscale is required.

## Linux deployment

With Go 1.26 or later, run `make binaries` and pick `dist/personal-mail-mcp-linux-amd64` or `dist/personal-mail-mcp-linux-arm64` for your server. The server needs a system CA bundle to verify TLS for IMAP, SMTP, and Microsoft OAuth. Put the binary, configuration, and secrets in paths readable by the service account:

```sh
cp config.example.json config.json
mkdir -p secrets
chmod 700 secrets
(umask 077; openssl rand -hex 32 > secrets/mcp-token)
```

Edit the accounts and interfaces in `config.json`. For the single-account example, put the Gmail app password in `secrets/personal-gmail` as a single line without Google's display spaces, and set its permissions to `0600`. Do not put secrets in command-line arguments, version control, or proxy logs.

```sh
./personal-mail-mcp-linux-amd64 -config config.json serve
```

Relative paths are resolved from the configuration file's directory. `interfaces` lists one or more network interface names; on Linux, use `ip -brief address` to find them. At startup the service binds the existing IPv4 and IPv6 unicast addresses of the selected interfaces, not wildcard addresses. Missing interfaces, missing usable addresses, and occupied ports fail startup. Restart after addresses change. Binding selects local destination addresses; use a firewall if you also need to restrict the physical ingress interface.

**The service does not provide HTTPS.** The example binds `lo` for local connections only. For remote access, use an HTTPS reverse proxy that forwards to `http://127.0.0.1:8080/mcp` and preserves `Authorization`. The MCP SDK accepts only loopback `Host` values on loopback connections by default, so a proxy can set the upstream `Host` to `127.0.0.1:8080`. Do not expose the plaintext endpoint to an untrusted network. **One token grants read, write, attachment-download, and send access across every configured account.** Generate a fresh token when migrating from a read-only deployment and give it only to trusted clients.

If your HTTPS proxy preserves the client-facing hostname (for example, Tailscale Serve), add `"additional_allowed_hosts": ["mail.example.test"]` to `config.json` and keep the service bound to `lo`. Specify DNS hostnames only, without schemes, paths, ports, trailing dots, or wildcards. Incoming requests match the entire hostname case-insensitively and may include a valid port. This setting applies only to loopback connections, does not trust `X-Forwarded-Host`, and does not replace Bearer authentication or the proxy's access policy.

## Mailbox configuration

Give each mailbox its own alias in `accounts`. Multiple accounts can use the same provider. Start with [`config.example.json`](config.example.json) for one mailbox, or see [`config.multi-account.example.json`](config.multi-account.example.json) for a complete example with Gmail, Google Workspace, Yahoo, QQ Mail, and Outlook together.

### Add and verify an account

1. On a new deployment, copy an example to `config.json`. On an existing deployment, merge only the desired entries into the existing `accounts` object; do not overwrite the other accounts or listener settings. Replace the fictional email addresses and remove unused entries. Use distinct aliases and credential files for multiple accounts with the same provider.
2. Generate credentials in the mailbox account: enable two-step verification and generate a Google app password for Gmail or Workspace (subject to organization policy); generate an app password in Yahoo Account Security; or enable IMAP/SMTP in QQ Mail and generate a client authorization code. Write each credential as a single line in its own `password_file`, readable only by the service account and authorized administrators. Do not commit credentials. For Outlook, use device authorization below instead of `password_file`.
3. Restart the service. Use `list_accounts` in your MCP client to confirm the alias and `list_folders` with `{"account":"personal-qq"}` (substitute your alias) to test IMAP. This does not send or modify mail, and it does not verify SMTP.

Adding a mailbox to an instance gives clients that already hold its Bearer token access to that mailbox, including write and send access. Put mailboxes that need isolation in separate instances with separate tokens.

### Provider settings

| `provider` | Default IMAP endpoint | Default SMTP endpoint | Credential |
| --- | --- | --- | --- |
| `gmail` | `imap.gmail.com:993` | `smtp.gmail.com:465` | App password |
| `workspace` | `imap.gmail.com:993` | `smtp.gmail.com:465` | App password, subject to organization policy |
| `outlook` | `outlook.office365.com:993` | Personal: `smtp-mail.outlook.com:587`; work or school: `smtp.office365.com:587` | Microsoft OAuth2; see below |
| `yahoo` | `imap.mail.yahoo.com:993` | `smtp.mail.yahoo.com:465` | App password |
| `icloud` | `imap.mail.me.com:993` | `smtp.mail.me.com:587` | Apple app-specific password |
| `qq` | `imap.qq.com:993` | `smtp.qq.com:465` | Authorization code generated after enabling IMAP/SMTP, not your QQ sign-in password |

For non-Outlook accounts, configure `provider`, `username`, and `password_file`. If needed, use `address` to override the implicit-TLS IMAP endpoint. If account policy disallows app passwords, the service does not fall back to another authentication method. A missing credential for one account does not prevent use of the others.

- `from_address` defaults to `username`. If your login is not a full email address, set a complete sender address permitted by the provider. For short iCloud usernames, the service appends `@icloud.com` for SMTP login and sending; use the full login name for other iCloud addresses.
- Port 465 uses implicit TLS by default; the port 587 endpoints above use STARTTLS. Set `smtp_address` and `smtp_tls` (`implicit` or `starttls`) to override them. Outlook accepts only the two Microsoft endpoints shown above, with STARTTLS. Both modes verify certificates and do not fall back to plaintext.
- `archive_folder`, `trash_folder`, and `drafts_folder` can be set to the exact folder names returned by `list_folders`. Otherwise, the service requires a unique folder marked `\Archive`, `\Trash`, or `\Drafts`, as applicable. It does not guess localized names or create those folders automatically. Moving, archiving, and trashing require native IMAP `MOVE`; there is no fallback that might permanently delete mail.

### Outlook

1. Register an app in [Microsoft Entra](https://entra.microsoft.com/) that supports the intended account type, and enable **Allow public client flows** in Authentication. During device authorization the MCP requests the Exchange Online delegated scopes `IMAP.AccessAsUser.All` and `SMTP.Send`, plus `offline_access`. If the portal does not list those two Exchange permissions, try device authorization without substituting Microsoft Graph permissions. Enable IMAP for a personal Outlook.com mailbox. Work or school accounts may require an administrator to allow IMAP, SMTP AUTH, device authorization, and consent. See Microsoft's [IMAP and SMTP OAuth documentation](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth).
2. Add the `personal-outlook` entry from the [multi-account example](config.multi-account.example.json) to `accounts`. Replace the username and `client_id` with your mailbox address and Application (client) ID. `tenant` defaults to `consumers`; for a work or school account, use the organization's tenant ID. Use a separate `oauth_file` for each mailbox, in a directory the service account can write to. You do not need a client secret.
3. On the server, run the following as the service user. Follow the prompt in your browser and verify that you are signing in to the intended mailbox. The saved token has mode `0600` and is refreshed when needed. Do not allow multiple processes to refresh the same token file. Stop the server before reauthorizing; reauthorize if you previously consented only to IMAP and now need SMTP.

   ```sh
   ./personal-mail-mcp-linux-amd64 -config config.json authorize-outlook personal-outlook
   ```

## Connect an agent

Use an MCP client that supports Streamable HTTP and custom request headers. A local configuration might look like this:

```json
{
  "mcpServers": {
    "personal-mail": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer YOUR_MCP_TOKEN" }
    }
  }
}
```

Configuration syntax varies by client. For remote access, use the proxy's HTTPS URL. The agent initiates all queries; there is no background polling, new-mail notification, or standalone event stream.

| Tool | What it does |
| --- | --- |
| `list_accounts` | List account aliases without returning usernames or credentials. |
| `list_folders` | List selectable folders for an account. |
| `search_messages` | Filter a folder (default `INBOX`) by `seen`, `starred`, `keyword`, `from`, `to`, `cc`, `subject`, `body`, `since`, or `before`. Arrival dates are inclusive for `since` and exclusive for `before`. |
| `search_mailboxes` | Use the same filters across up to eight explicit `{account, folder}` scopes, each with its own results, pagination, or error. Defaults to 10 results per scope; maximum 20. |
| `get_mailbox_status` | Return message count, unread count, and UIDVALIDITY for one to eight `{account, folder}` scopes. |
| `get_thread` | Follow exact `Message-ID`, `In-Reply-To`, and `References` relationships for a message, optionally across up to three additional folders in the same account. Returns folders and UIDs; `truncated` means results are incomplete. Does not match by subject. |
| `read_message` | Return plain text if available, otherwise unrendered HTML, plus read state, stars, recognized IMAP keywords, and the declared reply address. Does not mark the message as read. |
| `list_attachments` | Return filenames, media types, transfer-encoded sizes, and `part` identifiers. |
| `download_attachment` | Return decoded attachment data as `content_base64` for the selected `part`. Does not change read state. |
| `set_message_read` | Mark a message read (`seen: true`) or unread (`seen: false`). |
| `move_message` | Move a message to an existing `destination` folder in the same account. |
| `archive_message` | Move a message to the account's archive folder. |
| `delete_message` | Move a message to the trash without permanently deleting it. |
| `copy_message` | Copy a message to another existing folder in the same account, keeping the source. |
| `update_message_flags` | Change `seen`, `starred`, `add_keywords`, or `remove_keywords` for one to 50 `uids` in a folder with the same `uid_validity`, and verify the result by reading it back. |
| `move_messages` / `archive_messages` | Move or archive one to 50 UIDs from one source folder. Re-query unconfirmed UIDs instead of blindly retrying. |
| `create_folder` / `rename_folder` | Create or rename regular folders. Renaming may also move their children. |
| `retire_empty_folder` | Rename a confirmed empty leaf folder to a recoverable `MCP-Retired-…` name without IMAP DELETE. Use `rename_folder` to restore it. |
| `save_draft` / `list_drafts` / `discard_draft` | Save a new unsent draft, page through drafts, or move a draft to the trash. Saving neither overwrites a draft nor sends mail. |
| `send_message` | Send from `account` to `to` (an array of addresses), with a `subject` and plain-text `body`. Supports optional `cc`, `bcc`, and `attachments`. |
| `reply_message` | Reply to a message with plain-text `body` and optional attachments. Uses Reply-To or falls back to From. Does not reply-all or quote the original. Verify `reply_to` with `read_message` before sending. |
| `forward_message` | Forward a message to `to` with plain-text `body`, optional `cc` and `bcc`, and optional additional attachments. Attaches the original as `original.eml`. Refuses originals with nonempty `Bcc` or `Resent-Bcc` headers to avoid leaking recipients. |

For example, call `search_messages` with `{"account":"personal-gmail","seen":false,"limit":10}` to find unread mail without paging through and filtering all results. Omit `seen` or `starred` to leave that flag unrestricted. The upstream server matches body text. Custom keywords are standard IMAP flags, not Gmail labels.

Search returns UIDs in descending order. To paginate, send the previous page's `next_before_uid` as `before_uid`, along with its `uid_validity` and the original search conditions. One folder returns up to 50 messages per page; for cross-folder searches, put the pagination fields on the corresponding `scopes` entry. Summary fields and lists are bounded; `summary_truncated` signals that a limit was reached. If the result of saving a draft is uncertain, check its `message_id` via `list_drafts` before retrying. For message operations, use the original `account` and `folder`, plus the returned `uid` and `uid_validity`. After a move, search the destination again or use the new UID and UIDVALIDITY returned by the provider.

Send attachments as `{"filename":"notes.txt","media_type":"text/plain","content_base64":"aGVsbG8="}`; `media_type` is optional. Downloads are limited to 2 MiB decoded per attachment. Sending allows at most 10 attachments totaling 2 MiB, including `original.eml` when forwarding. Outgoing messages allow up to 25 recipients and 64 KiB of body text. SMTP success means the provider accepted the submission; whether it saves a Sent copy depends on the provider. If a submission result is uncertain, check before retrying.

MIME text parts over 256 KiB are not downloaded, and returned bodies are limited to 64 KiB. An IMAP request can read up to 8 MiB from upstream; the MCP request body limit is 4 MiB. Treat email bodies, reply addresses, and attachments as untrusted data, not as instructions.

## Provider compatibility

All five IMAP endpoints (Gmail and Workspace share one) advertised IMAP4rev1 in unauthenticated TLS `CAPABILITY` checks. Search, `STATUS` counts, copies, flags, folder renaming, and draft `APPEND` use standard IMAP commands. This confirms the protocol path, not the account's permissions or identical search and label semantics across providers.

| Provider | Observations and limits before sign-in |
| --- | --- |
| Gmail / Google Workspace | Did not advertise `MOVE` in the check. Gmail's [IMAP extensions](https://developers.google.com/workspace/gmail/imap/imap-extensions) expose labels as folders and supply separate label and thread identifiers; the service neither equates them with IMAP keywords nor depends on the extensions. Workspace policies can also block IMAP or app passwords. |
| Outlook | Advertised `MOVE` and `UIDPLUS`. Requires OAuth2 and is subject to administrator policy and mailbox permissions. |
| Yahoo | Advertised `MOVE`, `UIDPLUS`, and `SPECIAL-USE`. |
| iCloud | Did not advertise `MOVE` in the check. Support after sign-in was not verified. |
| QQ | Advertised `MOVE` and `UIDPLUS`. |

Moves run only if the current IMAP session supports native `MOVE`. There is no per-account compatibility diagnostic tool, and write, move, and delete operations were not verified against real mailboxes in these checks. Configure special-use folders manually if the provider does not mark them. Bulk flag updates run only when the folder advertises support for persisting the requested flags. The tool reads the results back and marks each UID `confirmed` or `unconfirmed`.

## Containers

`make image` produces `dist/personal-mail-mcp.oci.tar`, a Linux amd64/arm64 OCI image. Building requires Go and Docker Buildx with OCI export support; it does not pull a base image. The Makefile defaults to the build machine's CA bundle. Override it with `make image CA_FILE=/path/to/trusted-ca.pem`. The image contains no configuration or credentials.

On Linux, load the image and use host networking to bind real host interfaces. The service user must be able to read the mounted files; Outlook authorization and refresh also require write access to the token directory. Run from the directory containing `config.json` and `secrets/`:

```sh
docker load -i /path/to/personal-mail-mcp.oci.tar
docker run --rm --network host --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$PWD,dst=/config" personal-mail-mcp:local
```

For Outlook authorization in a container, stop the running service, then append `-config /config/config.json authorize-outlook personal-outlook` after the image name in the `docker run` command above. In bridge networking, `interfaces` names container interfaces; Docker port mappings or a proxy determine the host-side addresses.

Run `make test` for tests (including a local IMAP server, an MCP client, and mock OAuth endpoints) and `go vet`. Live mailbox authorization requires your own accounts.
