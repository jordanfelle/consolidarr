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
4. **Verify** once complete: probe each file, then check the pack covers every episode you currently have.
5. **Replace:** delete only the recorded old files, trigger Sonarr's import, confirm new files landed.

Nothing is deleted before step 5, and step 5 only runs when every check passes. Jobs that fail a check show as **holding**
with the reason. With *Dry run* on (the default), the scheduler only logs what it would grab.

## Configuration

| Variable | Default | |
|---|---|---|
| `CONSOLIDARR_LISTEN` | `:7480` | listen address |
| `CONSOLIDARR_DATA` | `/data` | data directory (SQLite lives here) |
| `CONSOLIDARR_DATABASE_URL` | | `postgres://…` to use Postgres instead of SQLite |
| `CONSOLIDARR_AUTH_DISABLED` | `false` | skip the login (only behind an authenticating reverse proxy) |

Everything else (scheduler, size cap, in-flight limit, last-resort indexers, anime) is in the UI under **Settings**.

## Development

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
