# npm Trusted Publishing

Publishing `@deepnoodle/mobius` to npm uses **OIDC trusted publishing** — no
`NPM_TOKEN` secret, no manual auth. GitHub Actions presents an OIDC identity;
npmjs.com validates it against a trusted-publisher config on the package and
issues a short-lived publish token automatically.

Getting this working the first time is fiddly. Every piece below is required.
If any one is wrong the registry returns a misleading `404 Not Found` on
`PUT /@deepnoodle%2fmobius` — there is no more specific error.

## Required pieces

### 1. Package exists on npm

OIDC trusted publishing can only be configured on an existing package. For the
very first version, publish manually from a local machine (`npm publish`) with
a logged-in account that owns the scope. Subsequent versions go through CI.

### 2. Trusted Publisher configured on npmjs.com

On `npmjs.com` → package page → **Settings → Trusted Publisher**:

- Publisher: **GitHub Actions**
- Organization or user: `deepnoodle-ai`
- Repository: `mobius`
- Workflow filename: `release-npm.yml` (filename only, no path)
- Environment name: `npm` (must match the `environment:` block in the workflow,
  or be blank if the workflow has no environment)

Every field must match exactly. A typo → 404.

### 3. Workflow permissions

```yaml
permissions:
  contents: write    # for gh release create
  id-token: write    # REQUIRED — lets the runner mint the OIDC token
```

### 4. GitHub Actions environment matches the trusted-publisher config

```yaml
environment:
  name: npm
  url: https://www.npmjs.com/package/@deepnoodle/mobius
```

If you drop this, also drop the Environment name on npmjs.com — they must agree.

### 5. Node 24+ (npm >= 11.5.1)

npm's built-in OIDC trusted-publishing flow landed in npm **11.5.1**. Node 20
ships with npm 10.x, which has no trusted-publishing support and silently falls
back to `.npmrc` auth — producing a 404. Node 24 ships with npm 11.

```yaml
- uses: actions/setup-node@v4
  with:
    node-version: "24"
    registry-url: https://registry.npmjs.org
```

### 6. Use `npm publish` for the publish step

The current workflow uses `npm publish` — known to work. `pnpm publish` may
also work on Node 24 (we never verified; every failing run we saw was on Node
20, so the pnpm failures were likely just the npm-version issue above). If you
switch to `pnpm publish`, test it end-to-end before relying on it.

Keep pnpm for `install` / `test` / `build`:

```yaml
- run: npm publish
```

No `--provenance`, no `NODE_AUTH_TOKEN` env. npm handles both automatically
under trusted publishing.

## Debugging 404s

The registry returns the same 404 regardless of which piece is broken. Check in
order:

1. Did `id-token: write` make it into the workflow?
2. Does the Trusted Publisher config on npmjs.com match the workflow
   (org/repo/filename/environment) character-for-character?
3. Is Node >= 24 in the publish job? Confirm with a step that runs `npm --version`
   — it must print `11.5.1` or higher.
4. Is the publish call `npm publish`? (`pnpm publish` is untested on this setup.)
5. Was the package ever published before? If not, do an initial manual publish.

## Reference

- Working counterpart: `.github/workflows/release-npm.yml`
