# scrubline

An undo timeline for the working tree your agent is editing.

```sh
npx scrubline watch      # record while your agent works
npx scrubline            # browse the timeline and restore
```

Full documentation: <https://github.com/nathanz3491/scrubline>

## What this package is

A shim, not a second implementation. `scrubline` is a single static Go binary;
this package downloads the build that matches your platform from the project's
GitHub releases, checks it against the published `checksums.txt`, and hands
every invocation straight to it.

It has no dependencies. The binary is fetched at install time when npm is
allowed to run install scripts, and otherwise on first use — which is the path
`npx` takes, since it never runs them.

If you would rather not go through npm at all:

```sh
curl -fsSL https://raw.githubusercontent.com/nathanz3491/scrubline/main/install.sh | sh
```

## Releasing (maintainers)

The npm package version must match the release tag, and publishing is a manual
step:

```sh
cd npm
npm version <the version just tagged> --no-git-tag-version
npm publish
```

Test it against a local build first, without touching the registry:

```sh
goreleaser release --snapshot --clean
cd npm && npm pack
cd /tmp && npm init -y
SCRUBLINE_BASE_URL="file://$OLDPWD/dist" SCRUBLINE_VERSION=v0.1.0-snapshot \
  npx --yes --package /path/to/scrubline-0.1.0.tgz -- scrubline version
```
