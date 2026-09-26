#!/usr/bin/env python3
"""Records the *arr API responses that the fixture spike did not capture (phase2-3.md §4.3).

The spike's containers no longer exist, so this script re-creates the part of their state each
missing response depends on, in fresh containers of the same versions (Sonarr 4.0.20.3014,
Radarr 6.4.4.10685, Lidarr 3.1.0.4875), and records:

  sonarr/episode-seriesId-1.json   GET /api/v3/episode?seriesId=1 (all seasons)
  sonarr/episode-seriesId-2.json   GET /api/v3/episode?seriesId=2
  lidarr/config-mediamanagement.json
  radarr/rootfolder-movies-4k.json  GET /api/v3/rootfolder with /movies and /movies-4k
  radarr/movie-movies-4k.json       GET /api/v3/movie: the spike's four movies plus movie 5 in
                                    /movies-4k (a root folder no path mapping covers), with a file
  radarr/movie-5.json               GET /api/v3/movie/5
  radarr/moviefile-movieId-5.json   GET /api/v3/moviefile?movieId=5

What is recorded and what is joined (every join is checked by internal/integrations/arr's
TestFixtureProvenance):

- Sonarr: both series are added exactly as in the spike (same order, so ids 1 and 2; tags
  bunkarr-full and irreplaceable; profile 6; monitor "all"), without files. The episode lists
  are recorded as they come. The spike's files then set episodeFileId and hasFile: for series 1
  the season 1 entries are replaced by the spike's own recording
  (episode-seriesId-1-season-1.json), after checking that every other field is identical; for
  series 2, S01E01 gets file 3 (episodefile-seriesId-2.json).
- sonarr/series-2.json is the second entry of series.json: GET series/{id} returns exactly the
  list entry (the recorded series-1.json, movie-1.json and artist-1.json prove it).
- Radarr: the four spike movies are added to /movies in the spike's order (ids 1-4), then movie 5
  (Nosferatu, 1922) to /movies-4k with a small real 2160p MKV that Radarr imports from disk. The
  list keeps the spike's recording of movies 1-4 and appends the recorded movie 5, whose file id
  becomes 5 (the spike used file ids 1-4). Nothing else is changed.
- Lidarr: config/mediamanagement of a fresh instance (the spike changed no media management
  setting: its Sonarr and Radarr recordings hold the defaults).

The API key is fixed below and appears in no recorded body (checked before writing).

Usage: python3 testdata/arr/record_slice2.py   (needs Docker, the three linuxserver images, the
jellyfin/jellyfin image for ffmpeg, and internet access for the *arr metadata lookups)
"""

import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
KEY = "0123456789abcdef0123456789abcdef"  # test-only key of the throw-away containers
PREFIX = "bunkarr-rec-"


def sh(*args, check=True, capture=True):
    r = subprocess.run(args, check=False, capture_output=capture, text=True)
    if check and r.returncode != 0:
        raise SystemExit(f"{' '.join(args)}: {r.stderr.strip()}")
    return r.stdout.strip() if capture else ""


def start(app, port, image, env_prefix):
    name = PREFIX + app
    sh("docker", "rm", "-f", name, check=False)
    sh("docker", "run", "-d", "--name", name, "-e", "PUID=1000", "-e", "PGID=1000", "-e", "TZ=Etc/UTC",
       "-e", f"{env_prefix}__AUTH__APIKEY={KEY}", "-p", f"127.0.0.1::{port}", image)
    host = sh("docker", "port", name, str(port)).splitlines()[0]
    return name, "http://" + host


class Arr:
    def __init__(self, base, prefix):
        self.base, self.prefix = base, prefix

    def req(self, method, path, body=None, raw=False):
        data = None if body is None else json.dumps(body).encode()
        r = urllib.request.Request(self.base + self.prefix + path, data=data, method=method,
                                   headers={"X-Api-Key": KEY, "Accept": "application/json",
                                            "Content-Type": "application/json"})
        with urllib.request.urlopen(r, timeout=120) as resp:
            b = resp.read()
        return b if raw else json.loads(b)

    def wait_up(self):
        for _ in range(180):
            try:
                self.req("GET", "/system/status")
                return
            except (urllib.error.URLError, ConnectionError, OSError, json.JSONDecodeError):
                time.sleep(1)
        raise SystemExit(f"{self.base} did not start")

    def wait_idle(self):
        time.sleep(2)
        for _ in range(300):
            busy = [c for c in self.req("GET", "/command") if c.get("status") in ("queued", "started")]
            if not busy:
                return
            time.sleep(1)
        raise SystemExit("commands did not finish")


def save(rel, raw):
    text = raw.decode() if isinstance(raw, bytes) else raw
    if KEY in text:
        raise SystemExit(f"{rel}: the API key appears in the body")
    doc = json.loads(text)
    with open(os.path.join(HERE, rel), "w") as f:
        json.dump(doc, f, indent=2, ensure_ascii=False)
        f.write("\n")
    print("wrote", rel)


def load(rel):
    with open(os.path.join(HERE, rel)) as f:
        return json.load(f)


def record_sonarr():
    name, base = start("sonarr", 8989, "lscr.io/linuxserver/sonarr:latest", "SONARR")
    try:
        a = Arr(base, "/api/v3")
        a.wait_up()
        sh("docker", "exec", name, "sh", "-c", "mkdir -p /tv && chown abc:abc /tv")
        a.req("POST", "/rootfolder", {"path": "/tv"})
        for label in ("bunkarr-full", "irreplaceable"):
            a.req("POST", "/tag", {"label": label})
        for tvdb, tag in ((71471, 1), (76479, 2)):
            s = a.req("GET", f"/series/lookup?term=tvdb:{tvdb}")[0]
            s.update({"qualityProfileId": 6, "languageProfileId": 1, "rootFolderPath": "/tv", "monitored": True,
                      "seasonFolder": True, "seriesType": "standard", "monitorNewItems": "all", "tags": [tag],
                      "addOptions": {"monitor": "all", "searchForMissingEpisodes": False,
                                     "searchForCutoffUnmetEpisodes": False}})
            a.req("POST", "/series", s)
            a.wait_idle()
        ep1 = a.req("GET", "/episode?seriesId=1", raw=True)
        ep2 = a.req("GET", "/episode?seriesId=2", raw=True)
    finally:
        sh("docker", "rm", "-f", name, check=False)

    # Series 1: the spike's season 1 recording carries its files.
    fresh1 = json.loads(ep1)
    spike_s1 = {e["id"]: e for e in load("sonarr/episode-seriesId-1-season-1.json")}
    out1 = []
    for e in fresh1:
        if e["seasonNumber"] == 1:
            s = spike_s1.pop(e["id"], None)
            if s is None:
                raise SystemExit(f"series 1 episode {e['id']} is not in the spike's season 1")
            a, b = dict(e), dict(s)
            for k in ("episodeFileId", "hasFile"):
                a.pop(k), b.pop(k)
            if a != b:
                raise SystemExit(f"series 1 episode {e['id']} differs from the spike: {a} != {b}")
            out1.append(s)
        else:
            if e["episodeFileId"] != 0 or e["hasFile"]:
                raise SystemExit(f"series 1 episode {e['id']} unexpectedly has a file")
            out1.append(e)
    if spike_s1:
        raise SystemExit(f"spike episodes missing from the new recording: {sorted(spike_s1)}")
    save("sonarr/episode-seriesId-1.json", json.dumps(out1))

    # Series 2: file 3 is S01E01 (episodefile-seriesId-2.json).
    files2 = load("sonarr/episodefile-seriesId-2.json")
    assert [(f["id"], f["seasonNumber"]) for f in files2] == [(3, 1)] and "S01E01" in files2[0]["relativePath"]
    out2 = json.loads(ep2)
    hit = [e for e in out2 if e["seasonNumber"] == 1 and e["episodeNumber"] == 1]
    assert len(hit) == 1 and hit[0]["episodeFileId"] == 0
    hit[0]["episodeFileId"], hit[0]["hasFile"] = 3, True
    save("sonarr/episode-seriesId-2.json", json.dumps(out2))

    save("sonarr/series-2.json", json.dumps(load("sonarr/series.json")[1]))


def make_mkv(out):
    name = PREFIX + "ffmpeg"
    sh("docker", "rm", "-f", name, check=False)
    sh("docker", "run", "--name", name, "--entrypoint", "/usr/lib/jellyfin-ffmpeg/ffmpeg", "jellyfin/jellyfin:12.1",
       "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=3840x2160:r=1/10",
       "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono", "-t", "5400", "-c:v", "libx264", "-preset", "ultrafast",
       "-crf", "51", "-c:a", "libopus", "-b:a", "6k", "-shortest", "/tmp/movie.mkv")
    sh("docker", "cp", f"{name}:/tmp/movie.mkv", out)
    sh("docker", "rm", "-f", name, check=False)


def record_radarr(tmp):
    mkv = os.path.join(tmp, "movie.mkv")
    make_mkv(mkv)
    name, base = start("radarr", 7878, "lscr.io/linuxserver/radarr:latest", "RADARR")
    try:
        a = Arr(base, "/api/v3")
        a.wait_up()
        sh("docker", "exec", name, "sh", "-c", "mkdir -p /movies /movies-4k && chown abc:abc /movies /movies-4k")
        a.req("POST", "/rootfolder", {"path": "/movies"})
        a.req("POST", "/rootfolder", {"path": "/movies-4k"})
        for label in ("bunkarr-full", "irreplaceable"):
            a.req("POST", "/tag", {"label": label})
        spike = load("radarr/movie.json")
        for m in spike:
            add(a, m["tmdbId"], "/movies", m["qualityProfileId"], m["monitored"], m["tags"])
        movie = add(a, 653, "/movies-4k", 5, True, [])
        if movie["id"] != 5:
            raise SystemExit(f"movie 5 got id {movie['id']}")
        folder = movie["path"]
        dest = f"{folder}/{os.path.basename(folder)} [Bluray-2160p].mkv"
        sh("docker", "exec", name, "mkdir", "-p", folder)
        sh("docker", "cp", mkv, f"{name}:{dest}")
        sh("docker", "exec", name, "chown", "-R", "abc:abc", folder)
        a.req("POST", "/command", {"name": "RescanMovie", "movieId": 5})
        a.wait_idle()
        if not a.req("GET", "/movie/5")["hasFile"]:
            raise SystemExit("Radarr did not import the 2160p file")
        roots = a.req("GET", "/rootfolder", raw=True)
        m5 = json.loads(a.req("GET", "/movie/5", raw=True))
        f5 = json.loads(a.req("GET", "/moviefile?movieId=5", raw=True))
    finally:
        sh("docker", "rm", "-f", name, check=False)

    old = m5["movieFileId"]
    m5["movieFileId"], m5["movieFile"]["id"] = 5, 5
    assert len(f5) == 1 and f5[0]["id"] == old
    f5[0]["id"] = 5
    save("radarr/rootfolder-movies-4k.json", roots)
    save("radarr/movie-5.json", json.dumps(m5))
    save("radarr/moviefile-movieId-5.json", json.dumps(f5))
    save("radarr/movie-movies-4k.json", json.dumps(load("radarr/movie.json") + [m5]))


def add(a, tmdb, root, profile, monitored, tags):
    m = a.req("GET", f"/movie/lookup/tmdb?tmdbId={tmdb}")
    m.update({"qualityProfileId": profile, "rootFolderPath": root, "monitored": monitored, "tags": tags,
              "minimumAvailability": "released", "addOptions": {"searchForMovie": False, "monitor": "movieOnly"}})
    out = a.req("POST", "/movie", m)
    a.wait_idle()
    return out


def record_lidarr():
    name, base = start("lidarr", 8686, "lscr.io/linuxserver/lidarr:latest", "LIDARR")
    try:
        a = Arr(base, "/api/v1")
        a.wait_up()
        save("lidarr/config-mediamanagement.json", a.req("GET", "/config/mediamanagement", raw=True))
    finally:
        sh("docker", "rm", "-f", name, check=False)


def main():
    which = set(sys.argv[1:]) or {"sonarr", "radarr", "lidarr"}
    with tempfile.TemporaryDirectory() as tmp:
        if "lidarr" in which:
            record_lidarr()
        if "sonarr" in which:
            record_sonarr()
        if "radarr" in which:
            record_radarr(tmp)


if __name__ == "__main__":
    main()
