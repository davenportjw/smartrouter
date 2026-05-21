---
name: session-notes
description: Use when starting any coding session — captures running session notes and maintains an executive summary across all sessions, highlighting content opportunities
---

# Session Notes

Maintain running markdown notes for every coding session and a rolling executive summary across sessions. Identify topics worth turning into public content.

**Announce at start:** Say to the user: "Starting session notes for this session."

## Session Startup

Run these steps at the beginning of every session, before doing any other work:

1. **Gitignore check.** Read `.gitignore` in the project root. If `.local/` is not listed, append a blank line and `.local/` to the end of the file. This is a one-time setup — skip if already present.

2. **Directory check.** Ensure `.local/sessions/` exists. Create it if it does not.

3. **Create session file.** Create `.local/sessions/YYYY-MM-DD-HHMM.md` using the current date and time (24-hour, zero-padded). Populate it with:

```markdown
# Session — YYYY-MM-DD HH:MM

**Branch**: `<current git branch>`
**Focus**: <one-line summary from the user's first message>

## Topics Covered

## Key Decisions

## Challenges

## Changes Made

## Open Questions
```

4. **Note the session file path** so you can update it throughout the session.

## During the Session

After completing a task, solving a bug, or making a significant decision, append to the appropriate section of the current session file. Keep entries concise — one or two lines each.

Good moments to update:

- A task is finished
- A bug is found and fixed
- A design choice is made
- Something surprising or difficult is encountered and resolved
- A meaningful code change is made

Do not interrupt the user's flow to update notes. Fold updates into natural pauses between tasks.

## Major Milestones

When a significant feature is completed, a hard bug is solved, or a major architectural decision is made, update the executive summary in addition to the session file. See the Executive Summary section below for what to update.

## Session End

When the user signals they are done, or the conversation is wrapping up:

1. Finalize the session file — fill in any sections that are still empty with "None this session" or relevant content. Ensure Open Questions captures anything unresolved.
2. Update the executive summary (see below).

## Executive Summary

The executive summary lives at `.local/sessions/SESSION-INDEX.md`. Create it on the first session if it does not exist. Update it at session end and at major milestones. The session file gets updated continuously; the executive summary gets updated only at major milestones and at session end.

Insert new rows at the top of each table (newest first).

The format:

```markdown
# Session Index

## Recent Sessions

| Date | Focus | Link |
|------|-------|------|
| YYYY-MM-DD HH:MM | Brief summary | [notes](YYYY-MM-DD-HHMM.md) |

## Active Threads

- Ongoing work that spans multiple sessions

## Key Decisions Log

- **YYYY-MM-DD** — Decision — rationale

## Content Opportunities

| Topic | Type | Session | Notes |
|-------|------|---------|-------|
| Example topic | blog | YYYY-MM-DD HH:MM | Why this is worth writing about |
```

### Content Opportunities

When you encounter any of these during a session, add them to the Content Opportunities table in the executive summary immediately — do not wait for session end if the opportunity is clear:

- A problem that took significant debugging effort and the root cause was non-obvious
- A pattern or technique that would help others facing similar problems
- An interesting architectural decision with clear trade-offs
- A creative workaround for a limitation in a tool, library, or API
- A topic where existing documentation or blog posts are lacking

Tag each with a type:
- **blog** — explanatory writing, deep dives, lessons learned
- **video** — visual walkthroughs, live coding, before/after demos
- **code-example** — standalone sample code, snippets, templates

## File Hygiene

- Session files and the executive summary are local-only. They must never be committed to the repository.
- The `.local/` directory is the designated location for all session data. The gitignore check at startup ensures this.
- Do not create session files outside `.local/sessions/`.
- Do not modify files outside `.local/` as part of this skill's operation (except appending to `.gitignore` on first run).
