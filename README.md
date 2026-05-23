# photo-copy-check

A small, parallel CLI that verifies every file in a **source** directory has a
byte-identical copy in a **destination** directory. Built for the "did my SD
card actually copy to disk before I wipe it?" moment.

Verification runs in three sequential phases, cheapest first. Each phase only
processes files that survived the previous one, so a broken copy fails fast
without ever paying for the hash pass:

1. **List comparison** — walk both trees once, diff the relative paths. Files
   only in the source are reported as `missing`.
2. **Byte-length comparison** — for files present in both, `stat` both sides
   in parallel and compare sizes. Mismatches are reported as `size_mismatch`.
3. **SHA-256 comparison** — for files whose sizes matched, hash both sides in
   parallel and compare. Mismatches are reported as `hash_mismatch`. Skipped
   entirely with `-quick`.

Phases 2 and 3 are dispatched to a worker pool sized to `runtime.NumCPU()` by
default, so big trees finish in roughly disk-read time rather than
file-by-file time.

## Build

Requires Go 1.20+.

```sh
go build -o photo-copy-check .
```

## Usage

```sh
./photo-copy-check -src <source-dir> -dst <copy-dir> [flags]
```

| Flag             | Default        | Description                                                     |
|------------------|----------------|-----------------------------------------------------------------|
| `-src`           | _(required)_   | Source directory (the original).                                |
| `-dst`           | _(required)_   | Copy directory to verify against the source.                    |
| `-workers`       | `NumCPU`       | Parallel workers. Must be `>= 1`.                               |
| `-images-only`   | `true`         | Only check known image extensions (see list below). Set `=false` to verify every file. |
| `-quick`         | `false`        | Compare file size only; skip hashing. Much faster, weaker guarantee. |

Symlinked roots are resolved via `EvalSymlinks` before walking, so passing a
symlinked source directory works as expected.

### Image extensions recognised by `-images-only`

`.jpg .jpeg .png .gif .bmp .tif .tiff .webp .heic .heif .raw .cr2 .nef .arw .dng .orf .rw2 .raf .srw .pef`

## Examples

Verify an SD card copy:

```sh
./photo-copy-check \
  -src /Volumes/Untitled/DCIM/100MSDCF \
  -dst ~/Pictures/2026-05-08-Spain
```

Verify every file (not just images), with explicit worker count:

```sh
./photo-copy-check -src ./src -dst ./backup -images-only=false -workers 8
```

Quick size-only sweep (useful as a first pass over a huge tree):

```sh
./photo-copy-check -src ./src -dst ./backup -quick
```

## Output

On success:

```
OK: all 2429 file(s) match.
```

On failure, a summary by category followed by one line per problem file:

```
FAIL: 3 file(s) had issues out of 2429.
  missing: 1
  hash_mismatch: 2

[hash_mismatch] DSC01234.ARW  (content differs)
[hash_mismatch] DSC01999.ARW  (content differs)
[missing]       DSC02050.JPG  (not present in copy)
```

Statuses:

- `missing` — file is in source but not in destination
- `size_mismatch` — sizes differ (likely a truncated/interrupted copy)
- `hash_mismatch` — same size, different bytes
- `read_error` — a stat or read failed (permissions, I/O error, etc.)

## Exit codes

| Code | Meaning                                                  |
|------|----------------------------------------------------------|
| `0`  | All files matched.                                       |
| `1`  | At least one file was missing/mismatched/unreadable.     |
| `2`  | Bad usage (missing flags, invalid `-workers`, bad path). |

Useful in scripts:

```sh
if ./photo-copy-check -src "$CARD" -dst "$BACKUP"; then
    diskutil eject "$CARD"
fi
```

## Notes & limitations

- Only walks files that exist in `-src`. Extra files in `-dst` are ignored
  (this is a copy verifier, not a sync diff).
- Symlinks *inside* the tree are not specially handled — they follow Go's
  `filepath.Walk` semantics (Lstat at the root, not followed during walk).
- `-quick` only checks size, so it cannot detect silent bit rot or partial
  writes of the same length. Use it as a fast first pass, not a guarantee.
