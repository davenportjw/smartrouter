# Session Notes

A skill that keeps running markdown notes for each coding session and maintains an executive summary across sessions. Works with any AI coding agent that can read markdown and write files (Claude Code, Gemini CLI, OpenCode, etc.).

## What it does

When a session starts, the skill creates a timestamped markdown file under `.local/sessions/` and populates it with the current branch and a focus line from the user's first message. As work happens, it appends to the session file at natural breakpoints: finished tasks, bugs found, decisions made, surprising problems.

At session end (and at major milestones), it updates an executive summary at `.local/sessions/SESSION-INDEX.md`. The summary tracks recent sessions, ongoing threads, key decisions, and content opportunities -- topics worth turning into blog posts, videos, or code examples.

All session data stays local. The skill adds `.local/` to `.gitignore` on first run so nothing gets committed.

## File layout

```
.skills/
  session-notes/
    SKILL.md          # The skill definition
    README.md         # This file

.local/
  sessions/
    SESSION-INDEX.md  # Executive summary across all sessions
    2026-05-21-1045.md
    2026-05-21-1422.md
    ...
```

## Setup

The skill is self-activating for agents that support skill auto-discovery from a `.skills/` directory. For agents that don't, add a one-liner to the agent's instruction file:

- **Claude Code**: Add `@.skills/session-notes/SKILL.md` to your CLAUDE.md
- **Gemini CLI**: Add `@.skills/session-notes/SKILL.md` to your GEMINI.md
- **OpenCode**: Point to the skill in your agent config

No other setup required. The skill handles directory creation and gitignore management on first run.

## Session file format

Each session file has these sections:

- **Topics Covered** -- what was discussed and decided
- **Key Decisions** -- choices made and the reasoning behind them
- **Challenges** -- what was hard or surprising
- **Changes Made** -- summary of what was modified
- **Open Questions** -- anything left unresolved

## Content opportunities

The executive summary has a dedicated section for topics that would make good public content. The skill tags these as they come up during sessions:

- **blog** -- deep dives, lessons learned, write-ups
- **video** -- visual walkthroughs, live coding demos
- **code-example** -- standalone samples, templates, snippets

Candidates include problems with non-obvious root causes, interesting architectural trade-offs, creative workarounds, and gaps in existing documentation.

## How it stays out of the way

Session notes update during natural pauses between tasks, not mid-flow. The skill won't interrupt you to ask about notes or prompt you for summaries. It folds updates into the gaps between work.
