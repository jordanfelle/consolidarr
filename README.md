# consolidarr

Sonarr is happy to fill a season one episode at a time, from whichever release group had it first. You end up with
seasons stitched together from five groups, three qualities and two sources. **consolidarr** finds those seasons,
ranks the messiest first, and replaces them with a proper season pack, only deleting your existing files once the pack
has downloaded, probed clean and been checked to cover every episode you have.

- **Multiple Sonarr instances** (HD, 4K, anime…), each with its own scan, queue health and path mapping
- **Ranked audit:** seasons with no season-pack files, scored by release-group, quality and source sprawl; TV before anime
- **Deterministic picking:** no guessing. Sonarr's own rejections are honoured, a pack must not be a downgrade or over your size cap, and chosen indexers (default TorrentLeech) are used only as a last resort
- **Verify, then delete:** `ffprobe` every file (catches DRM-encrypted and undecodable releases), confirm the pack covers every episode you have today, then delete the old files and trigger the import. Any failed check leaves your files alone
- **Scheduler** with a dry-run mode (on by default), an in-flight limit, and automatic pausing when Sonarr or the download client is unhealthy
- **SQLite or Postgres** for state; a single container; web UI with login

## Quick start

```yaml
services:
  consolidarr:
    image: ghcr.io/jordanfelle/consolidarr:latest
    container_name: consolidarr
    restart: unless-stopped
    ports:
      - "7480:7480"
    environment:
      TZ: Etc/UTC
      # Optional: Postgres instead of the bundled SQLite file
      # CONSOLIDARR_DATABASE_URL: postgres://consolidarr:password@postgres:5432/consolidarr?sslmode=disable
    volumes:
      - ./data:/data
      # Optional, recommended: your Sonarr download directory, read-only, for ffprobe verification
      # - /downloads:/downloads:ro
```

Open `http://localhost:7480`, create the admin account, then **Settings → Add instance** with your Sonarr URL
(including any URL base, e.g. `http://sonarr:8989/sonarr`) and API key. Run **Scan now** and browse **Seasons**.

### Verifying packs with ffprobe

Sonarr reports where a finished download lives *in Sonarr's container*. Mount that directory into consolidarr and fill
the two path fields on the instance (Sonarr's path and consolidarr's path; often identical). Without a mount,
consolidarr still checks episode coverage but cannot probe the files, and says so in the job message.

## How a replacement works

1. **Scan** reads every series' episode files through Sonarr's API. A season is flagged when none of its files came from a
   season pack, it has at least *Min files* files, and at least 80% of its aired episodes are on disk.
2. **Pick** searches Sonarr's indexers for full-season releases. Releases Sonarr rejects (blocklisted, too few seeders,
   unwanted quality…) are skipped, *except* "existing file meets cutoff", because the files are being replaced. Packs must not
   be a quality downgrade or exceed the size cap. Best quality then most seeders wins; last-resort indexers only if nothing else qualifies.
3. **Grab** records the season's current file IDs and tracks the download.
4. **Verify** once complete: list the files in the download's own folder, probe each with ffprobe (DRM, undecodable video),
   and check the pack covers every episode you currently have. If ffprobe can't reach the files the job is **held**, not
   deleted (unless you opt in to *Allow unverified*).
5. **Wait for a safe moment:** deletion is deferred while another season is still importing, while Sonarr has a monitor/import
   command running for 10+ minutes, more than 5 imports queued, or more than 100 completed downloads waiting to import.
6. **Replace:** delete only the recorded old files that still exist, trigger Sonarr's import, confirm new files landed.

Nothing is deleted before step 6, and step 6 only runs when every check passes. Jobs that fail a check show as **holding**
with the reason. With *Dry run* on (the default), the scheduler only logs what it would grab.

### Packs numbered in a different order

Sonarr numbers a season one way (usually aired order); a pack may number it another (DVD order, a wrong XEM scene
mapping, production codes). Importing such a pack normally files episodes under the wrong names, even when the counts
add up, so every verification compares each file's title with the episode Sonarr would give it. If they disagree:

1. If every episode you have today is identified exactly once by the titles in the file names (against Sonarr's titles,
   then against TVDB/TMDB orderings when keys are set), the pack is imported with an **explicit file-to-episode
   mapping**. Nothing is deleted first: Sonarr swaps each episode's file as it imports, so a failed import leaves your
   files as they were. Turn this off with *Import packs numbered in a different order* in Settings.
2. Otherwise the job is held with the disagreement spelled out.

Numbers alone are never evidence, and a title shared by two episodes (or a placeholder like "TBA") is ignored.

A pack held for a reason that cannot fix itself (it lacks episodes you have, has nothing importable, fails probing, or
is in another order that titles cannot settle) is removed and blocklisted after *Reject a stuck pack after (hours)*
(default 3) and the season is skipped for a week, so it stops occupying an in-flight slot.

## Configuration

| Variable | Default | |
|---|---|---|
| `CONSOLIDARR_LISTEN` | `:7480` | listen address |
| `CONSOLIDARR_DATA` | `/data` | data directory (SQLite lives here) |
| `CONSOLIDARR_DATABASE_URL` | | `postgres://…` to use Postgres instead of SQLite |
| `CONSOLIDARR_AUTH_DISABLED` | `false` | skip the login (only behind an authenticating reverse proxy) |
| `CONSOLIDARR_COOKIE_SECURE` | `auto` | session cookie `Secure` flag: `auto` sets it when the request is HTTPS (directly or via `X-Forwarded-Proto`), `always`, or `never` |
| `CONSOLIDARR_ALLOWED_HOSTS` | any | comma-separated Host names the server answers to (DNS-rebinding defence); others get 421. Cross-origin browser writes are always refused; API-key requests are exempt |
| `CONSOLIDARR_ALLOWED_IPS` | any | comma-separated client IPs/CIDRs (`10.0.0.0/24, 192.168.1.5, fd00::/8`) allowed to reach the UI and API; everyone else gets a bare 403. A malformed entry stops startup. IPv4 and IPv6 are matched separately (a `/0` admits that whole family and logs a warning); `::ffff:a.b.c.d/N` prefixes are treated as their IPv4 range. `/healthz` from the loopback is always answered for container healthchecks |
| `CONSOLIDARR_TRUSTED_PROXIES` | none | IPs/CIDRs of reverse proxies whose `X-Forwarded-For` is believed when applying `CONSOLIDARR_ALLOWED_IPS` (walked from the right; a header from any other peer is ignored). Only valid together with `CONSOLIDARR_ALLOWED_IPS`. The proxy must **append** to `X-Forwarded-For` (not pass the client's header through); entries may carry a port (`1.2.3.4:5678`, `[::1]:443`). The login rate limit then counts per real client instead of per proxy |
| `CONSOLIDARR_TRUSTED_ORIGINS` | none | extra origins accepted for browser writes, for a proxy that rewrites the Host header (otherwise writes get 403). With login disabled, dotted host names are refused unless listed in `CONSOLIDARR_ALLOWED_HOSTS`; IPs, `localhost` and single-label names always work |
| `CONSOLIDARR_TVDB_KEY` / `CONSOLIDARR_TVDB_PIN` | off | optional TheTVDB v4 key (and PIN for a subscriber key): lets consolidarr check a pack against TVDB's DVD/absolute/alternate orderings (see below) |
| `CONSOLIDARR_TMDB_KEY` | off | optional TMDB v3 key: same, using TMDB episode groups |

Everything else (scheduler, size cap, in-flight limit, last-resort indexers, anime) is in the UI under **Settings**.

## API

Everything the UI does is available over a JSON API. The interactive reference is served at `/api/docs` and the OpenAPI 3
spec at `/api/openapi.json` (a test fails if a route is added without documenting it).

Create a key under **Settings → API keys** (shown once; only a hash is stored), then:

```sh
curl -H "X-API-Key: csl_..." http://localhost:7480/api/status
curl -H "X-API-Key: csl_..." "http://localhost:7480/api/seasons?sort=score&limit=20"
curl -H "X-API-Key: csl_..." -X POST http://localhost:7480/api/scan
curl -H "Authorization: Bearer csl_..." http://localhost:7480/api/jobs?active=1
```

Errors always look like `{"error": "...", "code": "not_found"}`; request bodies reject unknown fields; list endpoints
paginate with `limit`/`offset`; creating things returns `201`; long operations (scans) return `202` and run in the background.

## Releases

Versions follow [semver](https://semver.org). Every merge to `main` publishes `ghcr.io/jordanfelle/consolidarr:main` and
`:latest` (for a deployment that tracks main; a tag never moves `:latest`); a `v*` tag publishes a pinned image and a
GitHub Release. A tag must be strict semver and on `main`, or the image job fails before publishing anything:

| Tag pushed | Image tags | Release |
|---|---|---|
| `v1.2.3` | `:1.2.3`, `:1.2`, `:1`, `:stable` | published, notes generated from the merged PRs |
| `v1.3.0-rc.1` | `:1.3.0-rc.1` only | marked prerelease; `:stable` and `:1.x` do not move |

(For `0.x` the `:0.1` tag is published but `:0` is not.) `:stable` always points at the newest non-prerelease, so a
deployment that wants released builds only can follow it instead of `:latest`.

Every merge to `main` also gets an immutable `:sha-<short>` tag (the short commit id). To roll a bad deploy back, pin
the compose image to the previous good one (`image: ghcr.io/jordanfelle/consolidarr:sha-3e6463b`), recreate, and fix
forward; unpin when the fix has merged.

To cut one, from an up-to-date, clean `main` whose CI is green:

```sh
scripts/release.sh v1.2.3 --dry-run   # checks everything and lists what would ship
scripts/release.sh v1.2.3
```

It refuses a version older than the newest stable release (that would move `:stable` backwards; there are no backport
releases, since it always tags `main`) and rejects any argument it does not know. CI independently refuses a tag that is
not strict semver or not on `main`, before anything is published.

Label PRs `bug`, `enhancement`, `dependencies` or `breaking` to have them grouped in the release notes
(`.github/release.yml`); unlabelled PRs land under "Other changes". The UI shows the running version, linked to the tag.

## Development

Tests that involve Sonarr use **recorded real responses** (`internal/sonarr/testdata`, see its README) plus the known
command shapes, never only a hand-made fake: a fake returns the shapes its author thought of, and the one that took
production down was not among them. The decoding contract is pinned in the `sonarr` package tests; the engine tests'
fake serves the same listing so the engine sees realistic data too. When a Sonarr response is part of a bug, add the real payload and a test that decodes
it. `go test ./...` runs with `-shuffle=on` in CI.

```
make web      # build the UI into web/dist
make test     # go vet + go test
make dev      # run with auth disabled and ./data
cd web && npm run dev   # UI dev server, proxies /api to :7480
```

## Notes

- Season-pack replacement re-downloads data you already have. A 12-episode season is often 20-30 GB. Set the size cap accordingly.
- Requires Sonarr v4 (episode files expose `releaseType`). Tested against 4.0.20.
- Not affiliated with Sonarr. Licensed under the [AGPL-3.0](LICENSE).
