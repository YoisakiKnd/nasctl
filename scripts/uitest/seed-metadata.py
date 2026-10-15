"""Add synthetic metadata-library rows only to an explicitly owned UI fixture."""
import argparse
import pathlib
import sqlite3

parser = argparse.ArgumentParser()
parser.add_argument("root", type=pathlib.Path)
parser.add_argument("--fixture-only", action="store_true", required=True)
args = parser.parse_args()
root = args.root.resolve()
assert (root / ".nasctl-ui-fixture").read_text() == "nasctl-ui-fixture-v1\n"
database = root / "state" / "anime.sqlite"
assert database.is_file()
db = sqlite3.connect(database)
existing = db.execute("SELECT title FROM subscriptions WHERE id=2").fetchone()
assert existing is None or existing[0] == "Generated Metadata Fixture"
db.execute("INSERT OR IGNORE INTO subscriptions(id,title,feed_url,season,enabled,auto_download) VALUES(2,'Generated Metadata Fixture','https://example.com/rss',1,0,0)")
series = root / "library" / "Generated Metadata Fixture"
(series / "Season 1").mkdir(parents=True, exist_ok=True)
(series / "tvshow.nfo").write_text("<tvshow>fixture custom series metadata</tvshow>")
for number in (1, 2):
    name = f"Generated Metadata Fixture S01E{number:02}"
    relative = f"Generated Metadata Fixture/Season 1/{name}.mp4"
    existing = db.execute("SELECT title FROM episodes WHERE id=?", (number + 1,)).fetchone()
    assert existing is None or existing[0] == f"Generated Metadata Fixture - {number:02}"
    db.execute("INSERT OR IGNORE INTO episodes(id,subscription_id,guid,title,url,number,season,state,library_path,updated_at,ever_submitted) VALUES(?,2,?,?,?, ?,1,'archived',?,1,1)", (number + 1, f"metadata-{number}", f"Generated Metadata Fixture - {number:02}", "https://example.com/generated.torrent", number, relative))
    (series / "Season 1" / (name + ".mp4")).write_bytes(b"generated original video bytes")
    (series / "Season 1" / (name + ".zh.srt")).write_text("generated original subtitle")
    (series / "Season 1" / (name + ".nfo")).write_text("<episodedetails>fixture custom episode metadata</episodedetails>")
db.commit()
db.close()
print("Generated metadata fixture: two archived records and original/video/subtitle/NFO bytes.")
