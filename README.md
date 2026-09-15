# scrubline

**An undo timeline for the working tree your agent is editing.**

Your agent was right three turns ago. Then it kept going, and now twelve files
are wrong and `git` has one messy WIP commit from an hour back. `Ctrl-Z` means
nothing across twelve files. `git stash` is not a time machine.

`scrubline` runs a tape deck over your working tree: it records a snapshot every
time you stop typing, and lets you wind back to any point and play from there.

![scrubline restoring a working tree](docs/demo.gif)

<sub>Every frame above is a view the tool itself rendered, against a real
repository; only the pacing between frames is chosen by the recorder. Reproduce
it with `SCRUBLINE_RECORD=1 go test ./internal/ui -run TestRecordDemo`.</sub>

```
$ scrubline watch
recording /home/you/project -- press Ctrl-C to stop
9f2c1a4b3e  0s ago    main          3 files
a71e04cc12  0s ago    main          1 file   turn 7: extract the parser

$ scrubline list
a71e04cc12  2m ago    main          1 file   turn 7: extract the parser
9f2c1a4b3e  6m ago    main          3 files
1b90ffe217  11m ago   main          2 files

$ scrubline restore 9f2c1a4b3e
restored 9f2c1a4b3e
  modify  src/parser.go
  delete  src/parser_v2.go
  add     src/lexer.go

the previous state is snapshot 4d1c0ba996 -- `scrubline restore 4d1c0ba996` puts it back
```

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/nathanz3491/scrubline/main/install.sh | sh
```

A single static binary with no runtime to install first, for macOS and Linux on
both Intel and ARM, plus Windows on Intel. Or:

```sh
go install github.com/nathanz3491/scrubline@latest     # if you have Go
```

```sh
brew install nathanz3491/tap/scrubline                 # once the tap is published
npx scrubline --help                                   # zero-commitment trial
```

Or download a binary from [the releases page](https://github.com/nathanz3491/scrubline/releases).

Uninstall is `rm ~/.local/bin/scrubline`. Snapshots live inside the repository
they belong to; `git update-ref -d refs/scrubline/timeline` removes them, and
`git gc --prune=now` reclaims the space.

## Use

```sh
scrubline watch                 # record continuously while you work
scrubline                       # browse the timeline
scrubline snap -m "before the refactor"
scrubline list                  # what have I got?
scrubline show <id> --files     # what did that snapshot change?
scrubline restore <id>          # put the tree back
scrubline restore <id> --dry-run --path src/parser.go
scrubline prune --older-than 7d # drop old snapshots
```

Leave `scrubline watch` running in a spare terminal while your agent works. A
burst of edits becomes one snapshot, not thirty.

### The browser

`scrubline` with no arguments opens the timeline: snapshots on the left, the
diff on the right.

| Key | |
|---|---|
| `↑` `↓` | move between snapshots |
| `r` | restore the selected snapshot, after a confirmation |
| `d` | toggle the diff between *vs now* and *vs previous* |
| `/` | filter snapshots by filename or label |
| `q` | quit |

`PgUp` / `PgDn` scroll a long diff. Colours adapt to light and dark terminals
and disappear entirely under `NO_COLOR`. Narrower than 56 columns the diff pane
is dropped rather than squeezed.

### For agent hooks

`scrubline mark "<label>"` labels a snapshot, so a timeline reads as *turns*
rather than as saves:

```
$ scrubline list
01b16191cf  1m ago    main   3 files  before: extract the tokenizer into its own module
7c02a11e3f  6m ago    main   1 file   before: make the parser handle empty input
b8d51d4373  9m ago    main   2 files  before: add the lexer
```

Now "the agent was right three turns ago" is something you can point at, and
restoring `7c02a11e3f` puts the tree back to just before the change that went
wrong.

`scrubline` is deliberately agent-agnostic — anything that can run a command at
a turn boundary can call it:

```sh
scrubline mark "before: $USER_PROMPT"
```

For **Claude Code** there is a drop-in pair of hooks that does this for you, in
[docs/claude-code.md](docs/claude-code.md). Copy it into `.claude/settings.json`
and your timeline reads as turns with no further setup.

## How it works

A snapshot is an ordinary git commit on a hidden ref, `refs/scrubline/timeline`,
staged through a private index file. That means:

- **It costs almost nothing.** Git already deduplicates and compresses; an
  unchanged file is stored once no matter how many snapshots contain it.
- **It cannot disturb your work.** Your index, `HEAD`, branches, and stashes are
  never touched. `git status` reads exactly the same before and after a snapshot.
- **It is not pushed anywhere.** The ref is local, outside `refs/heads`, so it
  does not travel with `git push` and does not appear in your branch list.
- **Diffing is free**, because everything is already a git object.

## What it will not do to you

Restore is the dangerous direction, so:

- **Every restore snapshots the current tree first.** The id is printed, and
  restoring it puts back every file scrubline manages. If a restore cannot be
  applied cleanly it is refused before anything is written, and on the rare
  failure that only shows up mid-write, the id you need is printed along with a
  plain statement that the tree is partly rewritten.
- **What your ignore rules match right now, scrubline does not touch** -- it is
  not recorded, not written, and not deleted. Your `node_modules/`, your `.env`,
  your build output, and your local database are not part of this. The rule is
  read fresh every time and applied in both directions, so adding a file to your
  ignore rules is enough to keep it out of scrubline's way from that moment on,
  including files that were snapshotted before you added it. The rule is your
  ignore *patterns*, whether or not git tracks the file -- a slightly larger set
  than the files git itself ignores, which Known limits spells out.
- **A plan that cannot be applied is refused whole.** A file that is now a
  directory, a read-only file, an unwritable directory: all of them stop the
  restore before the first write, list every problem at once, and leave the
  timeline clean -- no half-rewritten tree, and no safety snapshot minted for an
  attempt that did not happen.
- **`--dry-run` shows the file-level plan** and touches nothing. Plans longer
  than 40 paths list the first 40 and summarise the rest by operation, rather
  than filling your scrollback.
- **A `--path` that matches nothing is an error**, not a quiet success.
- **Nothing happens without a git repository.** `scrubline` refuses to run
  outside one rather than guessing what your project is.

## Known limits

These are real and worth knowing before you rely on restore:

- **Git LFS and custom clean filters.** Snapshots stage through `git add`, which
  runs your `.gitattributes` clean filter, and restore writes the stored blob
  back without the matching smudge filter. For LFS-managed paths that means a
  restore can replace a file with its pointer, and the safety snapshot holds the
  same pointer. `git lfs checkout` recovers the real content. If you use LFS or
  a custom clean filter, do not restore over those paths.
- **Windows line endings.** With `core.autocrlf=true` -- the Git for Windows
  default -- a CRLF file is stored as LF and restored as LF, so `git status`
  will show restored files as modified. Restore is byte-identical on macOS and
  Linux; on Windows it is line-ending-normalised.
- **Ignore rules are current, not historical.** Because the ignore decision is
  always "what do the ignore rules match right now", a snapshot is not guaranteed to
  reproduce your directory exactly if your ignore rules changed in between. The
  contract is that restore puts back the files scrubline manages -- not that it
  recreates your directory. The alternative would mean writing a secret back
  over a file you had just decided to ignore, which is worse.
- **Restoring past the creation of your `.gitignore` un-ignores what it
  covered.** `.gitignore` is an ordinary tracked file, so restoring to a
  snapshot from before it existed deletes it -- correctly -- and from that
  moment git no longer ignores the paths it listed. A `watch` left running will
  start recording them, with no prompt and no user action. This is the rule
  above behaving as specified rather than a bug, but it is worth knowing: if you
  restore that far back, put your `.gitignore` back before carrying on, or
  restore the safety snapshot the restore printed.
- **A tracked file that matches an ignore pattern is outside scrubline.** If
  `.gitignore` says `dist/` but `dist/index.html` is committed, git itself does
  not ignore that file -- `git status` reports your edits to it, and
  `git check-ignore` does not list it, because both consult the index. scrubline
  stages into a private index that starts empty, so ignore patterns are applied
  to every path regardless of what your repository tracks; the effective rule is
  `git check-ignore --no-index`. Such a file is therefore never recorded. It
  cuts both ways: it is never at risk from a restore, and equally a restore will
  not bring it back, while still reporting success. If you deliberately track
  files your ignore rules also match, they are not covered here.
- **A directory standing where a file belongs is refused, even when scrubline
  put every file in it.** Undoing an agent that split `utils.py` into a
  `utils/` package means replacing a directory with a file, and the restore
  refuses rather than clearing the directory -- it has no safe way to know that
  nothing else lives in there. Remove the directory yourself and run the restore
  again. This is a deliberate choice: the alternative is directory-removal logic
  on the restore path, and the cost of getting that subtly wrong is exactly what
  this tool exists to prevent.
- **One read-only file refuses the whole restore.** Pre-flight treats an
  unwritable file as a reason to refuse everything, which is stricter than
  `git checkout --`, which replaces a read-only file without complaint. A single
  vendored `chmod 444` config can therefore block a recovery. `chmod +w` it and
  retry. Refusing whole beats a half-written tree, but it is stricter than it
  strictly needs to be.
- **A case-only rename can lose the file, with `core.ignorecase=false` on a
  case-insensitive filesystem.** In that combination the index holds both
  spellings while the disk holds one file, and restoring to the snapshot with
  the other spelling produces a plan whose only action is to delete it. It
  reports success. The safety snapshot it prints recovers the content. This
  needs a non-default git setting: under the macOS default (`ignorecase=true`)
  git collapses the two spellings and the restore is a clean no-op.
- **Submodules are left alone.** A submodule pointer is never restored.

## Storage

Snapshots are git objects, so an unchanged file costs nothing no matter how many
snapshots contain it -- only what actually changed is stored, compressed. A
deliberately hostile measurement: 30 snapshots over a 40-file repository, each
one rewriting eight files with 20KB of fresh random text, came to 1.34 MiB. Real
editing sessions share far more between snapshots than that.

When it does get large:

```sh
scrubline prune --older-than 7d   # unlink old snapshots, always keeping the newest
git gc --prune=now                # reclaim the space
```

`prune` deliberately stops at unlinking. `git gc` prunes every unreferenced
object in your repository, not only scrubline's, and that is not a decision this
tool should make for you.

## Notes

- Pruning rebuilds the snapshots it keeps, so their ids change. Trees, labels,
  and timestamps do not.
- Don't point `scrubline watch`'s own output at a file inside the repository it
  is watching: each snapshot writes a log line, which is a change, which
  triggers the next snapshot.
- Linking in the browser's terminal UI library means a command probes the
  terminal for its background colour at startup -- but only when its output is a
  terminal, which a real one answers instantly. Piped or redirected, the way a
  hook runs it, nothing is emitted at all: measured at zero escape bytes on
  stdout and stderr for every command, which `TestHookOutputIsFreeOfEscapeCodes`
  keeps true.

## Requirements

`git` on your `PATH`, and a git repository. That is all.

## License

MIT. See [LICENSE](LICENSE).
