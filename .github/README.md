# Repo admin

## Author-identity enforcement (deferred)

GitHub rulesets that restrict commit author/committer email are not
available on free org-owned private repos. When this repo's plan supports
them (GitHub Pro / Team / Enterprise, OR if the repo becomes public),
apply [ruleset-author-identity.json](ruleset-author-identity.json) with:

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

The repo's local `.git/config` already sets `user.name` / `user.email`
to the correct identity, so plain `git commit` in this worktree
uses them. The local-only protection is `git config user.useConfigOnly`
(set to `true`) — refuses commits if `user.email` is somehow unset rather
than falling back to a default. To enable:

```sh
git config user.useConfigOnly true
```
