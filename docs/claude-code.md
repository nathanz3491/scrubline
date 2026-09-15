# Using scrubline with Claude Code

`scrubline` does not know anything about Claude Code, and this integration does
not change that. It is two hooks that call `scrubline mark` and `scrubline snap`
at turn boundaries, which turns the timeline from a list of saves into a list of
turns:

```
$ scrubline list
01b16191cf  1m ago    main   3 files  before: extract the tokenizer into its own module
7c02a11e3f  6m ago    main   1 file   before: make the parser handle empty input
b8d51d4373  9m ago    main   2 files  before: add the lexer
```

Now "the agent was right three turns ago" is a thing you can point at, and
`scrubline restore 7c02a11e3f` puts the tree back to just before you asked for
the change that went wrong.

## Install

Merge this into `.claude/settings.json` in your project, or
`~/.claude/settings.json` for every project. The full file is at
[claude-code-settings.json](claude-code-settings.json).

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "timeout": 10,
            "command": "command -v scrubline >/dev/null 2>&1 || exit 0; p=$(jq -r \".prompt // .user_prompt // empty\" 2>/dev/null | tr \"\\n\" \" \" | cut -c1-60); scrubline mark \"before: ${p:-a new turn}\" >/dev/null 2>&1; exit 0"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "timeout": 10,
            "command": "command -v scrubline >/dev/null 2>&1 && scrubline snap >/dev/null 2>&1; exit 0"
          }
        ]
      }
    ]
  }
}
```

If you already have hooks configured, add these entries to the existing
`UserPromptSubmit` and `Stop` arrays rather than replacing them.

## What each hook does

**`UserPromptSubmit`** snapshots the tree as it is *before* the turn runs, and
labels it with your prompt. That is the label you want when undoing: restoring
`before: make the parser handle empty input` puts you back to the moment before
you asked for that change, which is exactly what "undo that turn" means.

**`Stop`** takes an unlabelled snapshot when the turn finishes, so the last
turn's work is recorded even if you walk away rather than sending another
prompt. It is a plain `snap`, so it is a no-op when nothing changed.

You do not need `scrubline watch` running as well. Either is enough; hooks give
you turn boundaries, `watch` gives you finer granularity between them, and
running both is fine — a snapshot with nothing to record is a no-op.

## Why the commands look defensive

A hook that fails should never be the reason your session breaks, so each one:

- exits 0 unconditionally, so a failure can never block a turn;
- does nothing if `scrubline` is not installed;
- sends all output to `/dev/null`. This matters most for `UserPromptSubmit`,
  whose stdout is fed back into the model's context — an unsuppressed snapshot
  id would end up in your conversation;
- falls back to a generic label if `jq` is missing, rather than failing;
- exits quietly outside a git repository, so it is harmless in projects
  `scrubline` does not manage.

The prompt text is passed as a quoted shell variable rather than interpolated,
so a prompt containing quotes or backticks is recorded literally and cannot run
as a command.

## Verified, and what is not

Each command was tested by piping the hook payload to it directly, which is how
you test a `UserPromptSubmit` or `Stop` hook without waiting for one to fire:

```sh
echo '{"user_prompt":"fix the parser"}' | sh -c '<the command>'
scrubline list -n 1     # the label should be there
```

Confirmed that way: a payload carrying `user_prompt`, one carrying `prompt`,
one carrying both, one carrying neither, a prompt containing quotes, backticks
and newlines, a missing `scrubline`, a missing `jq`, and a directory that is not
a git repository. All exit 0 and print nothing; the ones with a prompt under
either name record it as the label.

**Not fired by a live session.** These hooks have been driven by hand with the
payload shape the hook documentation describes, not by Claude Code itself.

The prompt is read as `.prompt // .user_prompt` on purpose. Claude Code's hook
reference names the `UserPromptSubmit` field `user_prompt`; older notes call it
`prompt`. Reading both is correct under either spelling and costs nothing, which
retires the question rather than betting on an answer — and it is the only part
of this that a hand-written payload cannot settle, because a fixture you wrote
yourself tests the command against your own assumption about the name.

So the generic `before: a new turn` label should be rare: it means neither field
was present, or `jq` is not installed. The timeline still works either way; the
labels are just less useful.

To check a hook is live, run `/hooks` in Claude Code.

## Removing it

Delete the two entries from your settings file. Nothing else to undo — the
snapshots already taken are ordinary snapshots, and `scrubline prune` clears
them on the same schedule as any others.
