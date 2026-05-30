# Maintainers — repo administration

## Author-identity enforcement (deferred)

GitHub rulesets that restrict commit author/committer email are not
available on free org-owned private repos. When this repo's plan supports
them (GitHub Pro / Team / Enterprise, OR if the repo becomes public),
apply [ruleset-author-identity.json](.github/ruleset-author-identity.json) with:

```sh
gh api -X POST /repos/promise-language/flow/rulesets \
  --input .github/ruleset-author-identity.json
```

The ruleset rejects any push whose commits don't have BOTH author email
and committer email matching `11466501+djabi@users.noreply.github.com`.
Applies to all branches via `~ALL`.

To verify after applying:

```sh
gh api /repos/promise-language/flow/rulesets | jq '.[] | {name, target, enforcement}'
```

To remove (returns the ruleset id from the list above):

```sh
gh api -X DELETE /repos/promise-language/flow/rulesets/<id>
```

## Local enforcement

Until the server-side ruleset above can be applied, author identity is
enforced locally in three layers:

### 1. Pre-commit hook (primary gate)

The Forge pre-commit hook (`bin/precommit`, wired via `.githooks/pre-commit`)
rejects any commit whose **author OR committer** email is not a
`@users.noreply.github.com` address. It reads the impending identity with
`git var GIT_AUTHOR_IDENT` / `GIT_COMMITTER_IDENT`, so it catches both
`user.email` config and `GIT_*_EMAIL` env overrides.

The hook is installed automatically — `./make` runs `git config
core.hooksPath .githooks` on every invocation, so a fresh clone is protected
after its first bootstrap:

```sh
./make            # builds bin/, wires core.hooksPath -> .githooks
```

Verify it is active:

```sh
git config core.hooksPath          # -> .githooks
```

The enforcement logic lives in [`tools/build/common/precommit.go`](tools/build/common/precommit.go)
(`checkNoreplyIdentity`).

### 2. Correct identity in local config

The repo's local `.git/config` sets `user.name` / `user.email` to the
correct noreply identity, so plain `git commit` in this worktree uses them
and passes the hook.

### 3. useConfigOnly (belt-and-suspenders)

`git config user.useConfigOnly true` refuses commits if `user.email` is
somehow unset rather than falling back to a default:

```sh
git config user.useConfigOnly true
```
