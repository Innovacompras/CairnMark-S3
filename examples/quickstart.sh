#!/usr/bin/env bash
# End-to-end demo against a running CairnMark (default http://localhost:8080).
# Start the stack first:  docker compose up -d --build
set -euo pipefail

BASE="${CAIRNMARK_BASE:-http://localhost:8080}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "1) Upload a file with tags"
echo "the cairn marks the path" > "$TMP/note.txt"
RESP=$(curl -fsS -H "Content-Type: text/plain" \
  --data-binary @"$TMP/note.txt" \
  "$BASE/files?filename=note.txt&tag.project=cairnmark&tag.env=demo")
echo "$RESP"
ID=$(printf '%s' "$RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
echo "   -> id=$ID"

echo
echo "2) Fetch its metadata"
curl -fsS "$BASE/files/$ID/metadata"; echo

echo
echo "3) Download it (stream mode; default GET 302-redirects to a presigned URL)"
curl -fsS "$BASE/files/$ID?download=stream"

echo
echo "4) Search by tag — find every file tagged env=demo"
curl -fsS "$BASE/files?tag.env=demo"; echo

echo
echo "5) Add a tag, then delete"
curl -fsS -X PATCH -d '{"reviewed":true}' "$BASE/files/$ID/metadata"; echo
curl -fsS -o /dev/null -w "   delete -> HTTP %{http_code}\n" -X DELETE "$BASE/files/$ID"

echo
echo "6) Upload a zip, list what is inside, extract it (a job: submit, then poll), find one extracted entry"
if command -v zip >/dev/null; then
  mkdir -p "$TMP/docs"
  echo "quarterly numbers" > "$TMP/docs/q3.txt"
  echo '{"ok":true}' > "$TMP/docs/data.json"
  ( cd "$TMP" && zip -qr docs.zip docs )
  ARCHIVE=$(curl -fsS -H "Content-Type: application/zip" --data-binary @"$TMP/docs.zip" \
    "$BASE/files?filename=docs.zip" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  curl -fsS "$BASE/files/$ARCHIVE/archive"; echo
  JOB=$(curl -fsS -X POST "$BASE/files/$ARCHIVE/extract" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  echo "   -> job=$JOB (202 Accepted; the worker runs it — poll until it is final)"
  for _ in $(seq 1 60); do
    STATE=$(curl -fsS "$BASE/jobs/$JOB")
    case "$STATE" in
      *'"status":"succeeded"'*|*'"status":"failed"'*|*'"status":"cancelled"'*) break ;;
    esac
    sleep 1
  done
  echo "$STATE"
  curl -fsS "$BASE/files?tag.cm:archive_id=$ARCHIVE&tag.cm:archive_path=docs/q3.txt"; echo
else
  echo "   (zip is not installed; skipping)"
fi
