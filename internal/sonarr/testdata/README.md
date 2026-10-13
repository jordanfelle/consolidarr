# Sonarr API fixtures

`command.json`, `queue.json` (trimmed to 6 records), `history_grabbed.json` (a season's grab history, trimmed to 2 records, captured 2026-10-13, with each record's `data` object removed because it holds the indexer download URL and its API key), `system_status.json` and `health.json` are **real responses**
captured read-only from Sonarr 4.0.20.3014 on 2026-10-12. They contain no credentials (paths, release names and
download client names only).

`command_exotic.json` is hand-written from Sonarr's command resources: the kinds a quiet instance does not happen to
have in its listing at capture time (`RenameFiles` carries `files` as plain numbers, `ManualImport` as objects, some
commands have a `null` body). It exists because the listing mixes every command and one odd body once made the whole
listing undecodable in production, while every test passed against a hand-made fake that never sent such a command.

Rule of thumb: when a Sonarr response is involved in a bug, add the real payload here and a test that decodes it. Do
not refresh the real captures casually; a new capture should be a deliberate PR so a changed shape is visible in review.
