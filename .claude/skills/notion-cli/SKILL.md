---
name: notion-cli
description: 'Use when fetching a Notion page as Markdown via the `notion-cli` CLI tool. Triggers: reading a Notion article/page/doc by URL, dumping a Notion page to Markdown, exporting Notion content, summarizing a Notion page, converting Notion links to Markdown, checking notion-cli auth or config.'
---

# Notion CLI (`notion-cli`)

Reference for AI agents using the `notion-cli` tool to fetch Notion pages (via Notion's private/internal API, `token_v2`) and render them as Markdown. Built on the `notion-private-mcp` package.

## Install & Configure

### Prerequisites

Node.js 18+ required.

### Install

From the `notion-private-api-mcp` repo:

```bash
cd /path/to/notion-private-api-mcp
npm install
npm link
```

This makes the `notion-cli` command available globally.

### Get a token

The user needs their Notion session cookie (`token_v2`):

1. Log in at https://www.notion.so in a browser
2. Open DevTools → Application → Cookies → `https://www.notion.so`
3. Copy the value of the `token_v2` cookie

Treat it like a password — never print it back or commit it.

### Configure

Recommended (persists across shell sessions, like `cup init`):

```bash
notion-cli auth login --token 'your_token_v2'
```

Stored at `~/.config/notion-cli/config.json` (mode `600`). `notion-cli auth status` shows where the
active token is coming from (masked); `notion-cli auth logout` removes it.

Alternatives, in precedence order (each overrides the stored config token):

```bash
export NOTION_TOKEN_V2='your_token_v2'          # env var — highest precedence
```

Or create a `.env` file (with `NOTION_TOKEN_V2=...`) in the directory you run `notion-cli` from — it's loaded automatically.

### Verify

```bash
notion-cli auth status
```

If a command fails with `NOTION_TOKEN_V2 is required`, no token is set via any of the three methods above.

## Usage

```
notion-cli <page-url-or-id> [options]
```

Accepts any Notion page URL (`notion.so/...`, `app.notion.com/p/...`, with or without a `?source=` / `?pvs=` query string) or a bare page id (dashed or 32-char hex). The id is extracted from the URL automatically — no need to strip query params.

| Option | What it does |
| --- | --- |
| `-o, --output <file>` | Write Markdown to a file instead of stdout |
| `--json` | Print the raw page + full block/discussion/comment tree as JSON instead of Markdown |
| `--file` | Write Markdown to `~/.cache/notion-cli/<page-id>.md`, print only that path, and make image links relative to it. Use this for long pages instead of dumping the whole thing to stdout |
| `--no-images` | Omit images entirely (other file/bookmark links are kept) |
| `--link-images` | Render images as plain `[text](url)` links instead of `![]()` (default: embed) |
| `--no-local-images` | Link Notion's own signed, expiring URL instead of downloading images to `~/.cache/notion-cli/images` (default: download locally, since Notion's URLs won't resolve outside Notion) |
| `--no-comments` | Omit the trailing `## Comments` section |
| `--cacheless` | Bypass the on-disk cache entirely for this run (no read, no write) |
| `--reset-cache` | Clear the entire cache first, then make a fresh (cached) request |
| `-h, --help` | Show help |
| `-v, --version` | Show version |

Auth subcommands: `notion-cli auth login [--token <token_v2>]`, `notion-cli auth status`, `notion-cli auth logout`.

### Cache

Successful reads (`loadPageChunk`/`getRecordValues`) are cached on disk at `~/.cache/notion-cli/`
for 1 hour, shared across every `notion-cli` invocation and the MCP server (`get_page`/
`get_page_markdown`/etc.) — repeat lookups of the same page are near-instant once warm. A request
that fails (including `MemcachedCrossCellError`) is **never** cached, only ones that actually
succeeded are; writes (`create_page`, `append_blocks`, etc.) are never cached either. Use
`--cacheless` to force a live fetch for one run without touching the cache at all, or
`--reset-cache` to wipe it and start fresh (both flags go right after `notion-cli`, before the
page URL/id).

Output starts with a metadata line whenever the data has it:
`*Created: 2026-04-01 14:45 UTC · Last edited: 2026-07-07 08:39 UTC · Served from cache (cached 2026-07-22 11:44 UTC)*`.
The "Served from cache" part only appears if at least part of the page came from the cache above —
it's the **oldest** cache entry used, so with a mix of fresh and cached data it's the conservative
(older) timestamp, not necessarily when the whole page was last fetched.

### Examples

```bash
notion-cli https://www.notion.so/perfectpanel/llms-txt-file-348398726b558076bd14c1c966b120f2
notion-cli https://app.notion.com/p/perfectpanel/llms-txt-file-348398726b558076bd14c1c966b120f2?source=copy_link -o article.md
notion-cli 348398726b558076bd14c1c966b120f2 --json
```

Output is Markdown printed to stdout by default — pipe it, redirect it, or read it directly as an agent.

## Markdown mapping

| Notion block | Markdown |
| --- | --- |
| Heading 1/2/3 | `#` / `##` / `###` (page title itself is always the top `#`) |
| Bulleted / numbered list, to-do | `- `, `1. `, `- [ ]`/`- [x]` (nested items indented) |
| Toggle | `<details><summary>...</summary>...</details>` |
| Quote / callout | `> ` blockquote (callout keeps its emoji icon) |
| Code | fenced code block with language |
| Table | GFM table (first row rendered as the header row) |
| Divider | `---` |
| Image | embedded `![]()`, downloaded to the local image cache by default (see `--link-images` / `--no-local-images`) |
| File / video / bookmark / embed | plain Markdown link to the original URL |
| Sub-page / linked page | Markdown link to that page |
| Inline link to an external tool (ClickUp task, etc.) | Markdown link, e.g. `[Task #PNL-4293](https://app.clickup.com/...)` |
| `@mention` of a user | resolved to their real name (`@Jane Doe`), not a generic `@member` |
| Database / collection view | not supported — rendered as a note saying so; open it in Notion |

Bold/italic/strikethrough/inline-code/links are preserved; underline and text color have no Markdown equivalent and are dropped (text itself is kept).

Toggle blocks and toggleable headings (and any other block whose nested content Notion omits from
the initial page load) are automatically re-fetched so their content isn't silently dropped —
toggle content always renders inside `<details>`, and a toggleable heading's hidden body is
rendered directly under the heading (always expanded, not collapsed).

Comments render as a trailing `## Comments` section, one `### On: "<snippet of the commented block>"`
subsection per discussion, full thread in order (not just the first comment), each line as
`- **Author** (2026-04-17 12:00 UTC): comment text` (author name resolved, timestamp omitted if the
comment record doesn't have one). Verified against a live workspace with real multi-comment threads
(both resolved and open). Notion's
`discussion`/`comment` tables aren't officially documented, and the `comment` table specifically
requires the discussion's `space_id` in the request or it 500s with `MemcachedCrossCellError` — if a
page has visible comments that don't show up here, run `notion-cli <url> --json` and compare the
`discussions`/`comments` keys against what's expected in `renderComments()` in
`src/blocks-to-markdown.js`.

## Agent recipes

**A Notion link showed up in a PR description, branch name, commit message or ticket.**
Fetch it before doing anything else — it is almost always the spec:

```bash
notion-cli "<url>" --no-comments
```

Drop `--no-comments` when the discussion around the page matters (clarifications to the spec
often live only in comments).

**The page is long.** Write it to the cache and read the file instead of flooding the context:

```bash
notion-cli "<url>" --file        # prints only the path
```

**The page links to sub-pages.** Sub-pages render as Markdown links but their content is *not*
inlined. If a linked sub-page looks like part of the spec, fetch it separately.

**The page is a database / collection view.** Not supported — the CLI says so in the output.
Ask the user for a direct link to the specific page, don't guess at the contents.

**No token configured.** Report it and stop; don't try to obtain a `token_v2` yourself and never
echo a token into the transcript, a file or a commit.

## Troubleshooting

**`NOTION_TOKEN_V2 is required`** — no token set; run `notion-cli auth login`, or set the env var / `.env` file.

**My token stopped working** — `token_v2` expires when the Notion session ends (logout, password change, long inactivity). Get a fresh cookie and `notion-cli auth login` again.

**Database/collection pages** — not rendered as Markdown tables of rows; the CLI notes this and the user should open the page in Notion instead.

**Missing/wrong comments** — see the comments caveat above; compare `notion-cli <url> --json` against the real `discussions`/`comments` data.

**`Warning: Gave up loading N nested block(s)/discussion(s)/comment(s) after repeated
MemcachedCrossCellError`** (printed to stderr, page still prints) — a transient Notion routing
error on multi-cell workspaces (same one documented in the parent MCP server's README) that
persisted through several retries with backoff (~7.5s). The page's `## Comments` section or some
expanded toggle content may be incomplete. Just retry the command — it usually goes through on the
next attempt, and doesn't indicate a real problem with the page itself. `--json` includes the same
messages in a `warnings` array. If you don't see this warning, the page is complete.

**Is this against Notion's ToS?** It uses an undocumented internal API. Use only with your own account and data, at your own risk.
