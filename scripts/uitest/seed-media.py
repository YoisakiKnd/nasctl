"""Populate a named disposable media fixture. Never run against a deployed NAS.

The tiny placeholder video is for DOM/HTTP tests only, not codec validation.
An existing generated playable video and poster are retained for browser checks.
"""
import argparse
import pathlib
import sqlite3
import time

parser = argparse.ArgumentParser()
parser.add_argument("database", type=pathlib.Path)
parser.add_argument("library", type=pathlib.Path)
parser.add_argument("--fixture-only", action="store_true", required=True)
args = parser.parse_args()
if not args.database.is_file() or not args.library.is_dir():
    raise SystemExit("start the isolated anime service first")
db = sqlite3.connect(args.database)
title, feed = "Media Fixture", "https://example.com/ui-fixture.rss"
existing = db.execute("SELECT id,feed_url FROM subscriptions WHERE title=?", (title,)).fetchone()
if existing and existing[1] != feed:
    raise SystemExit("refusing to modify an unrelated subscription")
if existing:
    sub_id = existing[0]
else:
    sub_id = db.execute("INSERT INTO subscriptions(title,feed_url,season,enabled) VALUES(?,?,1,0)", (title, feed)).lastrowid
relative = "Media Fixture/Season 1/Media Fixture S01E01.mp4"
db.execute("""INSERT INTO episodes(subscription_id,guid,title,url,number,season,state,
library_path,updated_at,progress,ever_submitted,watch_position,watch_duration)
VALUES(?,'nasctl-ui-media-fixture','Media Fixture - 01','https://example.com/ui-fixture.torrent',
1,1,'archived',?,?,1,1,5,20) ON CONFLICT DO NOTHING""", (sub_id, relative, int(time.time())))
db.commit()
db.close()
directory = args.library / "Media Fixture" / "Season 1"
directory.mkdir(parents=True, exist_ok=True)
for name, content in {
    "Media Fixture S01E01.mp4": b"HTTP/DOM fixture; not a playable codec sample",
    "Media Fixture S01E01.chs.srt": "1\n00:00:00,000 --> 00:00:19,900\n字幕与续播测试\n".encode(),
    "Media Fixture S01E01.ass": b"[Script Info]\nTitle: Disposable fixture\n",
}.items():
    target = directory / name
    if not target.exists():
        target.write_bytes(content)
poster = args.library / "Media Fixture" / "poster.jpg"
if not poster.exists():
    poster.write_bytes(bytes([255, 216, 255, 217]))
print("Disposable media fixture ready; existing media files were retained.")
