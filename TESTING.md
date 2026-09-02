# Testing guide — Phases 1 & 2

A single pass that exercises everything built so far. Roughly 10 minutes.

Each step says **what you should see** and **what it proves**. If a step's
output differs from what's written here, that's a real finding — tell me.

---

## 0. Prerequisites

Docker Desktop must be running. Go 1.23+ is optional (only for `go test`).

> **If Docker Desktop won't start** — it failed for me twice during this build
> with `initializing Inference manager: ... The file cannot be accessed by the
> system`. Stale runtime sockets from an unclean shutdown. Fix:
>
> ```powershell
> Rename-Item "$env:LOCALAPPDATA\Docker\run" "$env:LOCALAPPDATA\Docker\run.stale"
> Rename-Item "$env:LOCALAPPDATA\docker-secrets-engine" "$env:LOCALAPPDATA\docker-secrets-engine.stale"
> ```
>
> Then start Docker Desktop. Both directories are recreated automatically.

> **If you're in Git Bash**, prefix `docker compose exec` commands with
> `MSYS_NO_PATHCONV=1`, or Git Bash rewrites `/photos` into a Windows path
> before it reaches the container. PowerShell and CMD don't need this.

---

## 1. Start clean

```bash
cd "C:/Users/srika/Documents/dev/photo dump cleaner"
docker compose down -v
docker compose up -d --build
```

**Expect:** three containers reach healthy/started — `postgres`, `api`, `worker`.

**Proves:** the whole stack builds and starts from scratch.

---

## 2. Generate the test corpus

```bash
go run ./cmd/genfixtures -root ./sample-photos -clean
```

**Expect:**

```
Wrote 46 files to ./sample-photos

  supported images        42
  unsupported (skipped)   4
  corrupt / undecodable   3

  exact duplicate groups  3  (9 files)
  near-duplicate groups   3

  with GPS                24
  without GPS             15
  without EXIF timestamp  6
```

**Proves:** the fixture generator produces a corpus with ground truth recorded
in `sample-photos/MANIFEST.json`. **Remember `supported images = 42`** — the
scan must find exactly that.

*(No local Go? Use `make fixtures`, which runs it in a container.)*

---

## 3. Health checks — Phase 1

```bash
docker compose exec api photo-organizer status
```

**Expect:**

```
API      http://localhost:8080
Process  OK (up 30s)
Database UP (0ms)
Libraries 0
```

```bash
curl -s http://localhost:8080/healthz
curl -s http://localhost:8080/readyz
```

**Expect:** `{"status":"ok",...}` from both.

**Proves:** API is up, migrations ran, database is reachable.

---

## 4. Schema and migrations — Phase 1

```bash
docker compose exec postgres psql -U photo -d photoorganizer -c '\dt'
docker compose exec postgres psql -U photo -d photoorganizer -c 'SELECT version, name, applied_at FROM schema_migrations'
```

**Expect:** tables `libraries`, `photos`, `schema_migrations`; exactly one
migration row (version 1, `core`).

**Proves:** migrations applied exactly once, embedded in the binary.

---

## 5. API survives a database restart — Phase 1

```bash
docker compose stop postgres
curl -s -o /dev/null -w 'healthz: %{http_code}\n' http://localhost:8080/healthz
curl -s -w '\nreadyz: %{http_code}\n' http://localhost:8080/readyz
```

**Expect:** `healthz: 200` and `readyz: 503`.

```bash
docker compose start postgres
# wait ~5 seconds
docker compose exec api photo-organizer status
```

**Expect:** `Database UP` again, and **uptime keeps climbing** — the API process
never restarted.

**Proves:** liveness deliberately does not check the database, so a database
blip doesn't cause a restart loop. Readiness does, and recovers on its own.

---

## 6. Migrations are immutable — Phase 1

```bash
echo "-- tampered" >> migrations/0001_core.sql
docker compose up -d --build api
# wait ~10 seconds
docker compose logs api --tail 3
```

**Expect:** the API refuses to start:

```
fatal: database: migration 0001_core was already applied but its contents
changed (recorded fa8dd4b31cad, on disk ...). Applied migrations are
immutable -- add a new one instead
```

**Restore it:**

```bash
git checkout migrations/0001_core.sql
docker compose up -d --build api
```

**Proves:** checksum drift detection — the API won't run against a schema that
doesn't match the repo.

---

## 7. Originals cannot be modified — Phase 1

```bash
docker compose exec api sh -c 'touch /photos/EVIL.txt'
docker compose exec api id
```

**Expect:** `Read-only file system`, and `uid=10001(photouser)`.

**Proves:** "never modify original photos" is enforced by the kernel, not by
convention. Container runs non-root.

---

## 8. Scan the folder — Phase 2

```bash
docker compose exec api photo-organizer scan /photos -wait
```

**Expect:**

```
Created library <uuid>
Root  /photos

Scan started.
  42 photos discovered. Scan complete.
```

**42 must match step 2's `supported images`.**

**Proves:** recursive discovery, format filtering, and that `-wait` polls to
completion. (That flag was broken initially — Go's `flag` package stops parsing
at the first positional argument, so `scan /photos -wait` silently ignored it.
Fixed, with a regression test.)

---

## 9. Rescanning is idempotent — Phase 2

Run the exact same command again:

```bash
docker compose exec api photo-organizer scan /photos -wait
docker compose logs api | grep "scan complete"
```

**Expect two lines** — the second is the important one:

```
... discovered=42 inserted=42 already_known=0  skipped_unsupported=4 ...
... discovered=42 inserted=0  already_known=42 skipped_unsupported=4 ...
```

**Proves:** re-scanning discovers the same files and inserts none of them. The
count reports it rather than the code claiming it. `skipped_unsupported=4`
matches the manifest's unsupported file count.

---

## 10. Path traversal is blocked — Phase 2

```bash
for p in /etc /photos/../etc ../../../../etc/passwd /var/lib/postgresql; do
  curl -s -o /dev/null -w "$p -> %{http_code}\n" \
    -X POST http://localhost:8080/api/libraries \
    -H 'Content-Type: application/json' -d "{\"path\":\"$p\"}"
done
```

**Expect:** every one returns `400`.

Check the error body:

```bash
curl -s -X POST http://localhost:8080/api/libraries \
  -H 'Content-Type: application/json' -d '{"path":"/etc"}'
```

**Expect:**

```json
{"error":{"code":"path_outside_root","message":"path must be inside the configured photo root"}}
```

**Note what's absent:** no filesystem path is echoed back. On a traversal
attempt that would confirm what does and doesn't exist outside the root.

And a legitimate relative path still works:

```bash
curl -s -o /dev/null -w 'trip -> %{http_code}\n' \
  -X POST http://localhost:8080/api/libraries \
  -H 'Content-Type: application/json' -d '{"path":"trip"}'
```

**Expect:** `201`. Clean it up:

```bash
docker compose exec postgres psql -U photo -d photoorganizer -c "DELETE FROM libraries WHERE root_path <> '/photos'"
```

---

## 11. Inspect what was found — Phase 2

```bash
docker compose exec api photo-organizer libraries
```

Copy the library id, then:

```bash
docker compose exec api photo-organizer photos <library-id> -limit 8
```

**Expect:** a table of photos. Things worth noticing:

- Paths use **forward slashes** and nest 3+ levels (`misc/nested/deep/...`)
- Filenames with **spaces and parentheses** are handled (`scene17 (1).jpg`)
- `MANIFEST.json`, `notes.txt`, `.mp4`, `.zip` are **absent** — correctly skipped
- `misc/empty.jpg` shows `0 B` — the zero-byte fixture was still discovered
- `scene17 (1).jpg` and `scene17-copy.jpg` are the **same size** — planted exact
  duplicates awaiting Phase 4

Pagination:

```bash
docker compose exec api photo-organizer photos <library-id> -limit 5 -offset 20
```

---

## 12. Interrupted scans recover — Phase 2

Simulate a crash mid-scan by marking a scan started and killing the API:

```bash
docker compose exec postgres psql -U photo -d photoorganizer \
  -c "UPDATE libraries SET last_scan_started_at = now(), last_scan_finished_at = NULL"
docker compose exec api photo-organizer libraries
```

**Expect:** scan state shows `scanning` (a stuck scan).

```bash
docker compose restart api
# wait ~8 seconds
docker compose logs api | grep reconciled
docker compose exec api photo-organizer libraries
```

**Expect:** a log line `reconciled scans interrupted by a previous shutdown`,
and the library back to `complete`.

**Proves:** a crashed scan doesn't permanently lock the library out of being
scanned again. A fresh process owns no scans by definition, so anything still
marked running was interrupted.

---

## 13. Automated tests

```bash
go test ./...
```

**Expect:** 5 packages pass — `cmd/cli`, `internal/config`, `internal/database`,
`internal/fixtures`, `internal/photos`.

Two symlink tests **skip on Windows** (creating symlinks needs elevation). To
run them on Linux:

```bash
make test
```

Integration tests against real Postgres:

```bash
make test-integration
```

**Expect:** 10 tests pass in `internal/ingest` — idempotency, batch boundaries,
cancel-and-resume, the scan guard, startup reconciliation.

---

## What is deliberately NOT working yet

Worth knowing so you don't report these as bugs:

| Thing | Status |
|-------|--------|
| `worker` container | **Stub.** Logs `worker is a Phase 1 stub` and idles. The job queue is Phase 5. |
| Photo `state` | All rows say `discovered`. Nothing derives metadata yet — Phase 3. |
| EXIF, GPS, dimensions | Not extracted. Phase 3. |
| Duplicate detection | Not implemented. The corpus contains 3 planted groups waiting for Phase 4. |
| Quality, clustering, thumbnails | Phases 6–8. |
| Web UI | Phase 10. |
| HEIC files | Not supported, by decision. See README limitations. |
| Corrupt files | Discovered as photos (they have image extensions) and will fail at decode in Phase 3. That's intended — 3 are planted in the corpus. |

---

## Teardown

```bash
docker compose down       # keep data
docker compose down -v    # delete the database volume too
```
