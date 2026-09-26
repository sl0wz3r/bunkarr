#!/usr/bin/env python3
"""Records the Tautulli, Seerr and Maintainerr fixtures of phase2-3.md §18 slice 9 (§4.4, §4.5,
§6.2) from pinned containers joined to a real, unclaimed Plex Media Server.

Everything under testdata/tautulli, testdata/seerr and testdata/maintainerr is written by this
script; each directory's index.json lists every file with the exact request that produced it
(method, path, query, auth), the answer's status and Content-Type, and whether the body is a
verbatim recording or was sanitized. No fixture is synthetic: every body is what the pinned
application answered. testdata/tautulli/plex/ holds the answers of the same PMS (the rating keys
and guids that the Tautulli history, the Seerr media and the Maintainerr collections refer to), so
that tests can join them the way the refresh does.

Images (pinned by index digest):
  PMS          plexinc/pms-docker 1.43.4.10903 (the Plex DB spike's pin)
  Tautulli     tautulli/tautulli v2.18.1   (and v2.17.2 for the version gate)
  Seerr        seerr/seerr v3.4.1          (and v3.3.0's status)
  Maintainerr  jorenn92/maintainerr 3.4.1  (and 3.4.0 and 3.3.0 for the version gate), and the
               latest release 3.29.0 under maintainerr/v3.29.0/

The scene (all on one user-defined bridge network, addressed by IP):
- PMS, unclaimed, with allowedNetworks set to the network's subnet (as in internal/e2e): every
  client on the subnet is admitted with or without a token, so the apps use a fake Plex token and
  no plex.tv account is involved. Its agents match the fake files online (tv.plex.agents.*), which
  gives the items real plex://, imdb://, tmdb:// and tvdb:// guids. Libraries: Movies (6 films),
  TV Shows (2 shows, 2 seasons each, 10 episodes), Music (2 artists, 6 tracks with local:// guids,
  one titled with HTML metacharacters).
  The files are 4 KiB of noise (video) or silent MPEG frames with ID3 tags (music).
- Tautulli: config.ini is written before the second start (PMS address, identifier, a fake PMS
  token, the fixed fake API key, logging_ignore_interval = 0 so that short sessions are logged,
  no update checks or analytics). Plays are real sessions: a fake player reports /:/timeline
  playing -> progress -> stopped to PMS, PMS shows the session in /status/sessions, Tautulli sees
  it over its websocket and writes history on stop. Without plex.tv, Tautulli has only its "Local"
  user (user_id 0), whose friendly name is set to one with HTML metacharacters so the escaping is
  recorded. keep_history is recorded on, then off (section 2 and the user) through edit_library and
  edit_user.
- Seerr: its setup wizard needs a plex.tv sign-in, so settings.json is marked initialized and
  pointed at the PMS, and the admin (user 1, a Plex user with a fake token) is inserted into the
  stopped instance's database. Two local users are then created through the API (one with a user
  name, one with only an e-mail). Requests are made through the API as those users (the recorder
  alone uses X-API-User for that; Bunkarr never sends it), approved or declined, then a Plex full
  scan marks the media in PMS available. A 4K Radarr at an address where nothing listens makes the
  4K request fail. Scheduled jobs are moved to 1 January so no scan races the requests.
- Maintainerr: configured through its API. Its rule executor sets collection visibility with
  POST /hubs/sections/<id>/manage, which an unclaimed PMS answers 404; Maintainerr 3.4 treats that
  as fatal and adds no media. Maintainerr therefore talks to PMS through a small forwarding shim
  that answers only that call itself (a minimal success body) and forwards everything else
  unchanged. The visibility flags are not part of any field Bunkarr decodes. The same collections
  are built in 3.4.1 and in 3.29.0 (which adds a Tautulli-based rule run again while Tautulli is
  stopped, and ServarrAction 5). The probes/ files come after the main set: Maintainerr's own
  collection handler, run with the recorded windows and with every window set to 0, shows which
  members Maintainerr itself acts on (its log is saved; the media are on a read-only mount, so no
  file is removed whatever PMS answers), and a global show exclusion shows how the exclusions are
  rewritten.

Sanitized: Tautulli's tautulli_platform_device_name (the container's random hostname) becomes
"bunkarr-fixture-tautulli"; the handler logs lose their timestamps and DEBUG lines. Every key and
token is fake by construction and is checked absent from every body before it is written; e-mail
addresses are fake (example.invalid) and are kept because the Seerr tests prove they are not
decoded.

Usage: python3 testdata/tautulli/record_slice9.py record   re-records every file (it first removes
       the files the indexes list), then runs the check
       python3 testdata/tautulli/record_slice9.py check    checks the recorded files offline: the
       index against the files, no secret, paging samples that concatenate to the full answers, and
       every rating key, guid and tmdb id joining the recorded PMS listings

Recording needs Docker and internet access (the Plex agents and Seerr's TMDB lookups), takes about
15 minutes, and removes its containers and volumes at the end (images are kept). The script re-runs
itself inside a python:3.13-slim container on the scene's network for every HTTP phase (Docker
Desktop does not route to bridge IPs from the host).
"""

import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# ---------------------------------------------------------------------------------------------
# Constants shared by the host orchestration and the in-network phases.

PLEX_IMAGE = "plexinc/pms-docker@sha256:e0ab27395614a8e1a4fdf84c6bc60ac664915cfdde70c52d030c7728a1c48e14"
TAUTULLI_IMAGE = "tautulli/tautulli:v2.18.1@sha256:7feb72192d5f477f2e4e28ed6b871b57478aff693c279462577cb4dad16556de"
TAUTULLI_OLD_IMAGE = "tautulli/tautulli:v2.17.2@sha256:864245fb24830ef6516b5f383bf4d8ba37939ae2f0574dc0217ca02fba4301ff"
SEERR_IMAGE = "seerr/seerr:v3.4.1@sha256:f4768de5f616248d723e05891f3345a1402123775d03bf0890dbfedc0831bda1"
SEERR_OLD_IMAGE = "seerr/seerr:v3.3.0@sha256:c92d2dc117f62185e7bcb88cd56efd374ea79210eaf433275449e8d5988eb5a8"
MAINTAINERR_IMAGE = "jorenn92/maintainerr:3.4.1@sha256:6d454b8a5dc5eaeec869d60d0e6da4d1e9fca13f102aa6914421a714a158f2c0"
MAINTAINERR_MIN_IMAGE = "jorenn92/maintainerr:3.4.0@sha256:efe5a335360d14093c0078bf2d710adf05f95a57adfbef6ae7988daa314bd59b"
MAINTAINERR_OLD_IMAGE = "jorenn92/maintainerr:3.3.0@sha256:918e375bf295f6a97ddfa0e6002b4b4b23adc47d118e54a8d0bbd4a4a5aa9bc8"
MAINTAINERR_LATEST_IMAGE = "jorenn92/maintainerr:3.29.0@sha256:7487d374290e59add65407ec295629e489f9057dbf18e2a9dff70d6201ffb6f9"
RUNNER_IMAGE = "python:3.13-slim@sha256:8d9d0b8bcf6506481eae4907c18f5e3e7902e629f5f6d684f9e7c32e85e3ddf0"

PREFIX = "bunkarr-rec9-"
NET = PREFIX + "net"
SUBNET = "172.29.9.0/24"
PLEX_ALLOWED = "172.29.9.0/255.255.255.0"
IP = {
    "plex": "172.29.9.10", "shim": "172.29.9.11",
    "tautulli": "172.29.9.20", "tautulli-old": "172.29.9.21",
    "maintainerr": "172.29.9.30", "maintainerr-min": "172.29.9.31", "maintainerr-old": "172.29.9.32",
    "maintainerr-latest": "172.29.9.33",
    "seerr": "172.29.9.40", "seerr-old": "172.29.9.41",
}
PLEX = "http://%s:32400" % IP["plex"]

# Fake credentials of the throw-away containers; none may appear in a recorded body.
TAUTULLI_KEY = "0123456789abcdef0123456789abcdef"
SEERR_KEY = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
PLEX_TOKEN = "fake-plex-token-for-fixtures"
PASSWORDS = ("fixture-password-1", "fixture-password-2")
SECRETS = (TAUTULLI_KEY, SEERR_KEY, PLEX_TOKEN) + PASSWORDS
TAUTULLI_FRIENDLY = "Local & <Fixture> \"Owner\""
TAUTULLI_DEVICE = "bunkarr-fixture-tautulli"

MOVIES = ["Nosferatu (1922)", "The General (1926)", "Metropolis (1927)", "Night of the Living Dead (1968)",
          "His Girl Friday (1940)", "Sherlock Jr. (1924)"]
SHOWS = ["The Twilight Zone (1959)", "Alfred Hitchcock Presents (1955)"]
EPISODES = ["S01E01", "S01E02", "S01E03", "S02E01", "S02E02"]
MUSIC = [("Scott Joplin", "Piano Rags", 1902, ["Maple Leaf Rag", "The Entertainer", "Elite Syncopations"]),
         ("Kevin MacLeod", "Classical Sampler", 2010, ["Gymnopedie No 1", "Canon in D", "Rags & <Riches> \"Live\""])]

# What the recording set up (written into each index.json). Rating keys are in plexIds.
SCENARIO = {
    "tautulli": {
        "sections": {"1": "Movies", "2": "TV Shows", "3": "Music"},
        "users": "only Tautulli's built-in Local user (user_id 0); its friendly name is set to "
                 "'Local & <Fixture> \"Owner\"' before the recording",
        "plays": [
            "Nosferatu: 3 sessions (95 %, 92 %, 99 %), two players",
            "The General: 1 session (97 %)",
            "Metropolis: 1 session stopped at 30 % (watched_status 0.25)",
            "The Twilight Zone S01E01: 2 sessions (95 %, 90 %); S01E02: 1 session (96 %)",
            "Alfred Hitchcock Presents S02E01: 1 session (95 %)",
            "Maple Leaf Rag: 2 sessions; Gymnopedie No 1: 1; 'Rags & <Riches> \"Live\"': 1",
        ],
        "keepHistory": "on for every section and the user in the main files; the *-keep_history-0 files "
                       "are after edit_library section 2 and edit_user user 0 set it off",
        "plexFiles": "plex/ holds the linked PMS's identity, sections and item listings",
    },
    "seerr": {
        "users": {"1": "admin, a Plex user (plexUsername fixture-admin, no username)",
                  "2": "local user with username fixture-local",
                  "3": "local user with only an e-mail (displayName is the e-mail)"},
        "requests": {
            "1": "movie tmdb 234, not in PMS, by user 2: pending (1)",
            "2": "movie tmdb 643, not in PMS, by user 2: declined (3)",
            "3": "movie tmdb 653 (Nosferatu), by the admin: approved, completed (5) by the Plex scan",
            "4": "movie tmdb 19 (Metropolis) in 4K, by the admin: sent to an unreachable 4K Radarr, failed (4)",
            "5": "movie tmdb 992 (Sherlock Jr.), by user 3: approved, completed (5) by the Plex scan",
            "6": "tv tmdb 6357 (The Twilight Zone) season 1, by user 2: approved (2), media partially available",
            "7": "tv tmdb 6357 season 2, by user 3: pending (1)",
            "8": "tv tmdb 21567 (The Outer Limits, 1963, not in PMS) seasons 1-2, by the admin: approved (2)",
            "9": "tv tmdb 5273 (Alfred Hitchcock Presents) seasons 'all' (stored as 1-7), by the admin: approved (2)",
            "10": "movie tmdb 3085 (His Girl Friday), by user 2: pending, completed (5) by the Plex scan",
        },
    },
    "maintainerr": {
        "collections": {
            "1": "Watched movies (movie, arrAction 0, 30 days): Plex viewCount > 0, plus Night of the Living "
                 "Dead added by hand (isManual)",
            "2": "Unmonitor only (movie, arrAction 3, 10 days): every movie",
            "3": "Do nothing (movie, arrAction 4, 10 days): every movie",
            "4": "No deadline (movie, arrAction 0, deleteAfterDays null): every movie",
            "5": "Inactive (movie, arrAction 0, 7 days): deactivated after the run (which empties it), then "
                 "His Girl Friday added by hand",
            "6": "Watched shows (show, arrAction 1, 14 days)",
            "7": "All seasons (season, arrAction 2, 21 days)",
            "8": "Watched episodes (episode, arrAction 0, 3 days)",
            "9": "v3.29.0 only: Tautulli played (movie, arrAction 0, 5 days): Tautulli viewCount > 0, run again "
                 "while Tautulli is stopped, so its members carry ruleEvaluationFailed",
            "10": "v3.29.0 only: Episodes, delete show if empty (episode, arrAction 5, 5 days)",
        },
        "exclusions": [
            "global: The General (a member of 1-4)",
            "rule group 6: show Alfred Hitchcock Presents (a member)",
            "rule group 7: show The Twilight Zone (its seasons are members)",
            "rule group 7: season Alfred Hitchcock Presents S02 (a member)",
            "rule group 8: season The Twilight Zone S01 (its episodes S01E01 and S01E02 are members)",
        ],
        "probes": "probes/ (and v3.29.0/probes/) are recorded after the main files: Maintainerr's own "
                  "collection handler run with the recorded windows (3.4.1 only) and with every set "
                  "deleteAfterDays changed to 0, as its log and the overlay-data after it; then a global "
                  "exclusion of show The Twilight Zone",
    },
}


HERE = os.path.dirname(os.path.abspath(__file__))
TESTDATA = os.path.dirname(HERE)
APPS = ("tautulli", "seerr", "maintainerr")


def log(*a):
    print(*a, flush=True)


# ---------------------------------------------------------------------------------------------
# HTTP helpers (in-network phases).

def http(method, url, body=None, headers=None, timeout=120):
    """Returns (status, content type, raw body); HTTP errors are answers, not exceptions."""
    data = None
    h = dict(headers or {})
    if body is not None:
        data = json.dumps(body).encode()
        h.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.headers.get("Content-Type", ""), r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Content-Type", ""), e.read()


def jhttp(method, url, body=None, headers=None, ok=(200, 201, 204)):
    st, ct, raw = http(method, url, body, dict({"Accept": "application/json"}, **(headers or {})))
    if st not in ok:
        raise SystemExit("%s %s: HTTP %d %s" % (method, url, st, raw[:500]))
    return json.loads(raw) if raw.strip() else None


def wait_for(what, fn, timeout=300, every=2):
    deadline = time.time() + timeout
    while True:
        try:
            v = fn()
            if v:
                return v
        except (OSError, urllib.error.URLError, ValueError, KeyError):
            pass
        if time.time() > deadline:
            raise SystemExit("timed out waiting for " + what)
        time.sleep(every)


# ---------------------------------------------------------------------------------------------
# Recording.

class Recorder:
    """Writes fixture files and the per-directory index (root is /out in a phase, testdata on the
    host)."""

    def __init__(self, app, sub="", root="/out"):
        self.app = app
        self.dir = os.path.join(root, app, sub)
        self.sub = sub
        os.makedirs(self.dir, exist_ok=True)
        self.index_path = os.path.join(root, app, "index.json")

    def _index(self):
        if os.path.exists(self.index_path):
            with open(self.index_path) as f:
                return json.load(f)
        return {"files": []}

    def record(self, name, method, base, path, query="", headers=None, body=None, auth="none",
               sanitize=None, note="", image=""):
        url = base + path + ("?" + query if query else "")
        st, ct, raw = http(method, url, body, headers)
        text = raw.decode("utf-8")
        sanitized = []
        if sanitize:
            text, sanitized = sanitize(text)
        for s in SECRETS:
            if s in text:
                raise SystemExit("secret %r in the answer of %s %s" % (s, method, url))
        try:
            doc = json.loads(text)
            ext = ".json"
            out = json.dumps(doc, indent=2, ensure_ascii=False) + "\n"
        except ValueError:
            ext = ".html" if "html" in ct else ".txt"
            out = text
        entry = {"method": method, "path": path, "query": query, "auth": auth, "status": st,
                 "contentType": ct, "image": image, "synthetic": False, "sanitized": sanitized}
        if note:
            entry["note"] = note
        self.save(name + ext, out, entry)
        return st, text

    def save(self, fname, text, entry):
        """Writes one file and its index entry (which gets the file's relative path)."""
        for s in SECRETS:
            if s in text:
                raise SystemExit("secret %r in %s" % (s, fname))
        with open(os.path.join(self.dir, fname), "w", encoding="utf-8") as f:
            f.write(text)
        idx = self._index()
        rel = os.path.join(self.sub, fname) if self.sub else fname
        idx["files"] = [e for e in idx["files"] if e["file"] != rel]
        idx["files"].append(dict({"file": rel}, **entry))
        with open(self.index_path, "w") as f:
            json.dump(idx, f, indent=2, ensure_ascii=False)
            f.write("\n")
        log("  recorded %s/%s (%s, %s)" % (self.app, rel, entry.get("status", "-"), entry.get("contentType", "-")))


# ---------------------------------------------------------------------------------------------
# In-network phases.

def phase_media():
    """Writes the fake library into the media volume (/media)."""
    import struct
    for t in MOVIES:
        d = "/media/movies/" + t
        os.makedirs(d, exist_ok=True)
        with open("%s/%s.mkv" % (d, t), "wb") as f:
            f.write(os.urandom(4096))
    for s in SHOWS:
        for e in EPISODES:
            d = "/media/tv/%s/Season %s" % (s, e[1:3])
            os.makedirs(d, exist_ok=True)
            with open("%s/%s - %s.mkv" % (d, s, e), "wb") as f:
                f.write(os.urandom(4096))

    def frame(fid, text):
        b = b"\x03" + text.encode()
        return fid.encode() + struct.pack(">I", len(b)) + b"\x00\x00" + b

    def syncsafe(n):
        return bytes([(n >> 21) & 0x7F, (n >> 14) & 0x7F, (n >> 7) & 0x7F, n & 0x7F])

    for artist, album, year, titles in MUSIC:
        d = "/media/music/%s/%s" % (artist, album)
        os.makedirs(d, exist_ok=True)
        for i, t in enumerate(titles, 1):
            frames = (frame("TPE1", artist) + frame("TPE2", artist) + frame("TALB", album) + frame("TIT2", t)
                      + frame("TRCK", str(i)) + frame("TYER", str(year)))
            tag = b"ID3\x04\x00\x00" + syncsafe(len(frames)) + frames
            audio = (b"\xff\xfb\x90\x44" + b"\x00" * 413) * 700  # ~18 s of silent MPEG-1 Layer III
            with open("%s/%02d - %s.mp3" % (d, i, t), "wb") as f:
                f.write(tag + audio)
    for root, dirs, files in os.walk("/media"):
        for x in dirs + files:
            os.chmod(os.path.join(root, x), 0o755 if x in dirs else 0o644)
    log("media written")


def plex_json(path):
    return jhttp("GET", PLEX + path)


def phase_plex():
    """Creates the libraries and waits until the agents have matched every item."""
    sections = [("Movies", "movie", "tv.plex.agents.movie", "Plex Movie", "/data/movies"),
                ("TV Shows", "show", "tv.plex.agents.series", "Plex TV Series", "/data/tv"),
                ("Music", "artist", "tv.plex.agents.music", "Plex Music", "/data/music")]
    for name, typ, agent, scanner, loc in sections:
        q = urllib.parse.urlencode({"name": name, "type": typ, "agent": agent, "scanner": scanner,
                                    "language": "en-US", "location": loc})
        # PMS answers /identity before its library accepts sections (400 for a few seconds). One
        # successful POST only: a retried success would create a second section.
        def create():
            st, _, _ = http("POST", PLEX + "/library/sections?" + q)
            return st in (200, 201)
        wait_for("section " + name, create, 120)
        log("section", name, "created")

    def matched():
        movies = plex_json("/library/sections/1/all?includeGuids=1")["MediaContainer"].get("Metadata", [])
        shows = plex_json("/library/sections/2/all?includeGuids=1")["MediaContainer"].get("Metadata", [])
        eps = plex_json("/library/sections/2/all?type=4")["MediaContainer"].get("Metadata", [])
        tracks = plex_json("/library/sections/3/all?type=10")["MediaContainer"].get("Metadata", [])
        ok = (len(movies) == len(MOVIES) and len(shows) == len(SHOWS) and len(eps) == len(SHOWS) * len(EPISODES)
              and len(tracks) == sum(len(m[3]) for m in MUSIC)
              and all(any(g["id"].startswith("tmdb://") for g in m.get("Guid", [])) for m in movies + shows)
              and all(e.get("duration") for e in eps))
        return ok and (movies, shows, eps, tracks)
    movies, shows, eps, tracks = wait_for("the agents to match every item", matched, 600, 5)
    ids = {"movies": {m["title"]: m["ratingKey"] for m in movies},
           "shows": {s["title"]: s["ratingKey"] for s in shows},
           "episodes": {"%s S%02dE%02d" % (e["grandparentTitle"], e["parentIndex"], e["index"]): e["ratingKey"] for e in eps},
           "seasons": {"%s S%02d" % (e["grandparentTitle"], e["parentIndex"]): e["parentRatingKey"] for e in eps},
           "tracks": {t["title"]: t["ratingKey"] for t in tracks},
           "machineIdentifier": plex_json("/identity")["MediaContainer"]["machineIdentifier"]}
    with open("/work/plex-ids.json", "w") as f:
        json.dump(ids, f, indent=2)
    log(json.dumps(ids, indent=1))


def load_ids():
    with open("/work/plex-ids.json") as f:
        return json.load(f)


def phase_tautulli_config():
    """Edits a stopped Tautulli's config.ini (volume at /c). argv: api_enabled (1|0)."""
    enabled = sys.argv[3] if len(sys.argv) > 3 else "1"
    ids = load_ids()
    p = "/c/config.ini"
    with open(p) as f:
        s = f.read()
    sets = {"api_key": TAUTULLI_KEY, "api_enabled": enabled, "first_run_complete": "1", "check_github": "0",
            "check_github_on_startup": "0", "system_analytics": "0", "launch_browser": "0",
            "logging_ignore_interval": "0", "pms_ip": IP["plex"], "pms_port": "32400", "pms_token": PLEX_TOKEN,
            "pms_identifier": ids["machineIdentifier"], "pms_url": PLEX, "pms_name": "bunkarr-fixture-plex",
            "pms_is_remote": "0", "pms_platform": "Linux"}
    for k, v in sets.items():
        s, n = re.subn(r"(?m)^%s = .*$" % k, "%s = %s" % (k, v), s)
        if n != 1:
            raise SystemExit("config.ini: %s found %d times" % (k, n))
    with open(p, "w") as f:
        f.write(s)
    log("tautulli config written (api_enabled=%s)" % enabled)


def tautulli(cmd, base="http://%s:8181" % IP["tautulli"], **params):
    q = urllib.parse.urlencode(dict({"cmd": cmd}, **params))
    return jhttp("GET", base + "/api/v2?" + q, headers={"X-Api-Key": TAUTULLI_KEY})["response"]


PLAYER_HEADERS = {"X-Plex-Product": "Bunkarr fixture player", "X-Plex-Platform": "Linux",
                  "X-Plex-Version": "1.0", "X-Plex-Device": "Linux", "X-Plex-Provides": "player",
                  "Accept": "application/json"}


def play(rk, frac, client, hold=3):
    """One real session: playing at 1 s, progress to frac of the duration, stopped."""
    h = dict(PLAYER_HEADERS, **{"X-Plex-Client-Identifier": client, "X-Plex-Device-Name": client})
    meta = plex_json("/library/metadata/%s" % rk)["MediaContainer"]["Metadata"][0]
    d = int(meta.get("duration") or 1800000)
    end = int(d * frac)

    def tl(state, t):
        q = urllib.parse.urlencode({"ratingKey": rk, "key": "/library/metadata/%s" % rk, "state": state,
                                    "time": t, "duration": d})
        jhttp("GET", PLEX + "/:/timeline?" + q, headers=h)
    tl("playing", 1000)
    time.sleep(4)
    tl("playing", end)
    time.sleep(hold)
    tl("stopped", end)
    time.sleep(4)
    log("  played", rk, meta.get("title"), frac, client)


def phase_plays():
    ids = load_ids()
    wait_for("Tautulli's API", lambda: tautulli("get_tautulli_info")["result"] == "success", 120)
    wait_for("Tautulli's websocket", lambda: tautulli("get_libraries")["data"], 60)
    m, e, t = ids["movies"], ids["episodes"], ids["tracks"]
    # Section 1: Nosferatu 3 plays (two players), The General 1, Metropolis 1 partial (30 %).
    # Section 2: Twilight Zone S01E01 twice, S01E02 once; Hitchcock S02E01 once.
    # Section 3: Maple Leaf Rag twice, Gymnopedie once, the track whose title has HTML metacharacters
    # once (tracks need >= 15 s of real play time).
    plan = [(m["Nosferatu"], 0.95, "player-1", 3), (m["Nosferatu"], 0.92, "player-2", 3),
            (m["The General"], 0.97, "player-1", 3), (m["Metropolis"], 0.30, "player-1", 3),
            (e["The Twilight Zone S01E01"], 0.95, "player-1", 3), (e["The Twilight Zone S01E01"], 0.90, "player-2", 3),
            (e["The Twilight Zone S01E02"], 0.96, "player-1", 3), (e["Alfred Hitchcock Presents S02E01"], 0.95, "player-2", 3),
            (m["Nosferatu"], 0.99, "player-1", 3),
            (t["Maple Leaf Rag"], 0.95, "player-1", 14), (t["Gymnopedie No 1"], 0.90, "player-2", 14),
            (t["Maple Leaf Rag"], 0.97, "player-2", 14), (t['Rags & <Riches> "Live"'], 0.95, "player-1", 14)]
    for rk, frac, client, hold in plan:
        play(rk, frac, client, hold)
    n = wait_for("13 history rows", lambda: tautulli("get_history", length=100)["data"]["recordsTotal"] == 13, 60)
    log("history rows written", n)


def tautulli_sanitize(text):
    new = re.sub(r'("tautulli_platform_device_name": )"[^"]*"', r'\1"%s"' % TAUTULLI_DEVICE, text)
    return new, (["data.tautulli_platform_device_name (container hostname)"] if new != text else [])


def history_query(section, start, length):
    return urllib.parse.urlencode([("cmd", "get_history"), ("section_id", section), ("media_type", "movie,episode,track"),
                                   ("grouping", "0"), ("include_activity", "0"), ("order_column", "date"),
                                   ("order_dir", "asc"), ("start", start), ("length", length)])


def phase_record_tautulli():
    base = "http://%s:8181" % IP["tautulli"]
    hdr = {"X-Api-Key": TAUTULLI_KEY}
    r = Recorder("tautulli")
    img = TAUTULLI_IMAGE

    def rec(name, query, note="", headers=hdr, auth="header"):
        return r.record(name, "GET", base, "/api/v2", query, headers, auth=auth, sanitize=tautulli_sanitize,
                        note=note, image=img)
    # The friendly name carries HTML metacharacters so that the escaping is recorded.
    tautulli("edit_user", user_id="0", friendly_name=TAUTULLI_FRIENDLY, keep_history="1", allow_guest="0")
    rec("get_tautulli_info", "cmd=get_tautulli_info")
    rec("get_server_info", "cmd=get_server_info", note="pms_identifier is the linked PMS's machineIdentifier (plex/identity.json)")
    for s in ("1", "2", "3"):
        rec("get_library-section_id-" + s, "cmd=get_library&section_id=" + s)
    rec("get_users", "cmd=get_users", note="only the built-in Local user (user_id 0) exists without plex.tv")
    for s in ("1", "2", "3"):
        rec("get_history-section_id-" + s, history_query(s, 0, 1000), note="the refresh's exact query (§4.4)")
    for start in (0, 2, 4):
        rec("get_history-section_id-1-length-2-start-%d" % start, history_query("1", start, 2),
            note="paging sample: 5 rows of section 1 read 2 at a time")
    rec("get_history-section_id-1-length-2-start-6", history_query("1", 6, 2), note="past the end: an empty page")
    # History off for section 2 and for the user.
    tautulli("edit_library", section_id="2", keep_history="0")
    tautulli("edit_user", user_id="0", friendly_name=TAUTULLI_FRIENDLY, keep_history="0", allow_guest="0")
    rec("get_library-section_id-2-keep_history-0", "cmd=get_library&section_id=2", note="after edit_library keep_history=0")
    rec("get_users-keep_history-0", "cmd=get_users", note="after edit_user keep_history=0")
    # Errors.
    rec("error-no-key", "cmd=get_tautulli_info", headers={}, auth="none")
    rec("error-bad-key", "cmd=get_tautulli_info", headers={"X-Api-Key": "ffffffffffffffffffffffffffffffff"}, auth="header (wrong key)")
    rec("error-unknown-cmd", "cmd=get_bunkarr_fixture")
    rec("error-history-bad-section", history_query("99", 0, 1000), note="a section Tautulli does not know")


def phase_record_tautulli_disabled():
    r = Recorder("tautulli")
    r.record("error-api-disabled", "GET", "http://%s:8181" % IP["tautulli"], "/api/v2", "cmd=get_tautulli_info",
             {"X-Api-Key": TAUTULLI_KEY}, auth="header", sanitize=tautulli_sanitize,
             note="config.ini api_enabled = 0", image=TAUTULLI_IMAGE)


def phase_record_tautulli_old():
    base = "http://%s:8181" % IP["tautulli-old"]
    wait_for("Tautulli 2.17", lambda: http("GET", base + "/api/v2?cmd=get_tautulli_info&apikey=" + TAUTULLI_KEY)[0] == 200, 120)
    r = Recorder("tautulli")
    r.record("get_tautulli_info-v2.17.2", "GET", base, "/api/v2", "cmd=get_tautulli_info", {"X-Api-Key": TAUTULLI_KEY},
             auth="header", sanitize=tautulli_sanitize, image=TAUTULLI_OLD_IMAGE,
             note="version gate: 2.17 answers the header-only key like this")
    # The recorder alone sends the key in the query (Bunkarr never does), to show the version.
    st, ct, raw = http("GET", base + "/api/v2?cmd=get_tautulli_info&apikey=" + TAUTULLI_KEY)
    text, san = tautulli_sanitize(raw.decode())
    r.save("get_tautulli_info-v2.17.2-query-key.json", json.dumps(json.loads(text), indent=2, ensure_ascii=False) + "\n",
           {"method": "GET", "path": "/api/v2", "query": "cmd=get_tautulli_info&apikey=<key>", "auth": "query (recorder only)",
            "status": st, "contentType": ct, "image": TAUTULLI_OLD_IMAGE, "synthetic": False, "sanitized": san,
            "note": "the key is shown as <key> in this entry; the body holds none"})


def phase_record_plex():
    """The linked PMS's answers (for joins in tests): identity, sections and every item listing."""
    r = Recorder("tautulli", "plex")
    h = {"Accept": "application/json"}

    def rec(name, path, query="", note=""):
        r.record(name, "GET", PLEX, path, query, h, note=note, image=PLEX_IMAGE)
    rec("identity", "/identity")
    rec("library-sections", "/library/sections")
    for key, types in (("1", ("1",)), ("2", ("2", "3", "4")), ("3", ("8", "9", "10"))):
        for t in types:
            rec("library-sections-%s-all-type-%s" % (key, t), "/library/sections/%s/all" % key,
                "type=%s&includeGuids=1&X-Plex-Container-Start=0&X-Plex-Container-Size=500" % t,
                note="§4.6 AllItems, one page")
    for start in (0, 4, 8):
        rec("library-sections-2-all-type-4-start-%d-size-4" % start, "/library/sections/2/all",
            "type=4&includeGuids=1&X-Plex-Container-Start=%d&X-Plex-Container-Size=4" % start,
            note="paging sample: 10 episodes read 4 at a time")


# --- Seerr --------------------------------------------------------------------------------------

def phase_seerr_seed():
    """Seeds a stopped Seerr's settings.json and database (volume at /c)."""
    import sqlite3
    ids = load_ids()
    p = "/c/settings.json"
    with open(p) as f:
        s = json.load(f)
    s["main"]["apiKey"] = SEERR_KEY
    s["main"]["mediaServerType"] = 1  # Plex
    s["main"]["versionCheck"] = False
    s["public"]["initialized"] = True
    s["plex"] = {"name": "bunkarr-fixture-plex", "ip": IP["plex"], "port": 32400, "useSsl": False,
                 "machineId": ids["machineIdentifier"],
                 "libraries": [{"id": "1", "name": "Movies", "enabled": True, "type": "movie"},
                               {"id": "2", "name": "TV Shows", "enabled": True, "type": "show"}]}
    s["radarr"] = [{"id": 0, "name": "unreachable-4k-radarr", "hostname": "172.29.9.250", "port": 7878,
                    "apiKey": "fake-radarr-key", "useSsl": False, "baseUrl": "", "activeProfileId": 1,
                    "activeProfileName": "Any", "activeDirectory": "/movies-4k", "is4k": True,
                    "minimumAvailability": "released", "isDefault": True, "externalUrl": "", "syncEnabled": False,
                    "preventSearch": True, "tags": [], "tagRequests": False}]
    for job in s["jobs"].values():
        job["schedule"] = "0 0 0 1 1 *"
    with open(p, "w") as f:
        json.dump(s, f, indent=1)
    c = sqlite3.connect("/c/db/db.sqlite3")
    c.execute("insert into user (id,email,username,plexId,plexToken,permissions,avatar,userType,plexUsername) "
              "values (1,?,?,?,?,?,?,?,?)",
              ("fixture-admin@example.invalid", None, 1000001, PLEX_TOKEN, 2, "", 1, "fixture-admin"))
    c.commit()
    c.close()
    log("seerr seeded")


def seerr(method, path, body=None, as_user=None, ok=(200, 201, 204)):
    h = {"X-Api-Key": SEERR_KEY}
    if as_user:
        h["X-API-User"] = str(as_user)  # recorder only
    return jhttp(method, "http://%s:5055/api/v1%s" % (IP["seerr"], path), body, h, ok)


def phase_seerr():
    wait_for("Seerr", lambda: seerr("GET", "/auth/me")["id"] == 1, 120)
    u2 = seerr("POST", "/user", {"email": "local-user@example.invalid", "username": "fixture-local",
                                 "password": PASSWORDS[0], "permissions": 32})
    u3 = seerr("POST", "/user", {"email": "email-only@example.invalid", "password": PASSWORDS[1], "permissions": 32})
    assert (u2["id"], u3["id"]) == (2, 3), (u2["id"], u3["id"])

    def request(body, as_user=None):
        b = seerr("POST", "/request", body, as_user)
        log("  request %d: %s -> status %d" % (b["id"], json.dumps(body), b["status"]))
        return b["id"]
    # tmdb ids: 234 Caligari and 643 Potemkin are not in PMS; 21567 The Outer Limits (1963) is not either.
    a = request({"mediaType": "movie", "mediaId": 234}, 2)                                   # 1 pending
    b = request({"mediaType": "movie", "mediaId": 643}, 2)                                   # 2 -> declined
    request({"mediaType": "movie", "mediaId": 653})                                          # 3 approved -> completed
    request({"mediaType": "movie", "mediaId": 19, "is4k": True})                             # 4 4K -> failed
    e = request({"mediaType": "movie", "mediaId": 992}, 3)                                   # 5 -> approved -> completed
    f = request({"mediaType": "tv", "mediaId": 6357, "tvdbId": 73587, "seasons": [1]}, 2)    # 6 -> approved
    request({"mediaType": "tv", "mediaId": 6357, "tvdbId": 73587, "seasons": [2]}, 3)        # 7 pending
    request({"mediaType": "tv", "mediaId": 21567, "seasons": [1, 2]})                        # 8 approved
    request({"mediaType": "tv", "mediaId": 5273, "tvdbId": 73614, "seasons": "all"})         # 9 approved
    request({"mediaType": "movie", "mediaId": 3085}, 2)                                      # 10 pending -> completed
    assert a == 1
    seerr("POST", "/request/%d/decline" % b)
    seerr("POST", "/request/%d/approve" % e)
    seerr("POST", "/request/%d/approve" % f)
    time.sleep(5)  # the 4K request's send to the unreachable Radarr fails asynchronously
    seerr("POST", "/settings/jobs/plex-full-scan/run")
    time.sleep(3)
    wait_for("the Plex scan", lambda: not any(j["id"] == "plex-full-scan" and j["running"] for j in seerr("GET", "/settings/jobs")), 300)
    time.sleep(3)
    got = [(x["id"], x["status"]) for x in seerr("GET", "/request?take=100&skip=0&sort=added&sortDirection=asc")["results"]]
    want = [(1, 1), (2, 3), (3, 5), (4, 4), (5, 5), (6, 2), (7, 1), (8, 2), (9, 2), (10, 5)]
    if got != want:
        raise SystemExit("Seerr request statuses %s, want %s" % (got, want))
    log("seerr requests:", got)


def phase_record_seerr():
    base = "http://%s:5055" % IP["seerr"]
    hdr = {"X-Api-Key": SEERR_KEY, "Accept": "application/json"}
    r = Recorder("seerr")

    def rec(name, path, query="", headers=hdr, auth="header", note=""):
        return r.record(name, "GET", base, "/api/v1" + path, query, headers, auth=auth, note=note, image=SEERR_IMAGE)
    rec("status", "/status", headers={"Accept": "application/json"}, auth="none")
    rec("auth-me", "/auth/me")
    rec("request-take-100-skip-0", "/request", "take=100&skip=0&sort=added&sortDirection=asc",
        note="the refresh's exact query (§4.5); Seerr has no 'added' sort and orders by request id")
    for skip in (0, 4, 8):
        rec("request-take-4-skip-%d" % skip, "/request", "take=4&skip=%d&sort=added&sortDirection=asc" % skip,
            note="paging sample: 10 requests read 4 at a time")
    rec("user-take-100-skip-0", "/user", "take=100&skip=0")
    for skip in (0, 2):
        rec("user-take-2-skip-%d" % skip, "/user", "take=2&skip=%d" % skip, note="paging sample")
    rec("error-bad-key", "/auth/me", headers={"X-Api-Key": "ZmFrZS1iYWQta2V5", "Accept": "application/json"},
        auth="header (wrong key)")
    rec("error-no-key", "/auth/me", headers={"Accept": "application/json"}, auth="none")


def phase_record_seerr_old():
    base = "http://%s:5055" % IP["seerr-old"]
    wait_for("Seerr 3.3", lambda: http("GET", base + "/api/v1/status")[0] == 200, 120)
    Recorder("seerr").record("status-v3.3.0", "GET", base, "/api/v1/status", "", {"Accept": "application/json"},
                             auth="none", image=SEERR_OLD_IMAGE, note="an unconfigured 3.3.0")


# --- Maintainerr --------------------------------------------------------------------------------

# Maintainerr variants: the contract's version (3.4.1, recorded at the top of testdata/maintainerr)
# and the latest release (3.29.0, under v3.29.0/), whose collection members carry includedByRule,
# manualMembershipSource and ruleEvaluationFailed and whose ServarrAction adds 5-7.
MT = {
    "3.4.1": {"ip": "maintainerr", "container": "maintainerr", "image": MAINTAINERR_IMAGE, "sub": ""},
    "3.29.0": {"ip": "maintainerr-latest", "container": "maintainerr-latest", "image": MAINTAINERR_LATEST_IMAGE, "sub": "v3.29.0"},
}


def mt_base(variant):
    return "http://%s:6246" % IP[MT[variant]["ip"]]


def mt(variant, method, path, body=None, ok=(200, 201)):
    b = jhttp(method, mt_base(variant) + "/api" + path, body, None, ok)
    if isinstance(b, dict) and b.get("code") == 0:
        raise SystemExit("Maintainerr %s %s %s: %s" % (variant, method, path, b))
    return b


def mt_rule(app, prop, action, value):
    """firstVal [application, property] with a number customVal. Applications: 0 Plex, 4 Tautulli."""
    return {"operator": None, "firstVal": [app, prop], "action": action,
            "customVal": {"ruleTypeId": 0, "value": str(value)}, "section": 0}


def mt_group(name, lib, data_type, rules, arr_action, delete_after_days):
    return {"libraryId": lib, "name": name, "description": "bunkarr fixture: " + name, "isActive": True,
            "arrAction": arr_action, "useRules": True, "dataType": data_type, "rules": rules,
            "listExclusions": False, "forceSeerr": False, "notifications": [],
            "collection": {"visibleOnHome": False, "visibleOnRecommended": False, "deleteAfterDays": delete_after_days,
                           "manualCollection": False, "manualCollectionName": "", "keepLogsForMonths": 6}}


# Properties (GET /api/rules/constants, the same ids in 3.4.1 and 3.29.0): Plex 5 viewCount,
# 14 sw_episodes, 17 sw_amountOfViews; Tautulli 3 viewCount. Actions: 0 BIGGER, 1 SMALLER.
# ServarrAction: 0 DELETE, 1 UNMONITOR_DELETE_ALL, 2 UNMONITOR_DELETE_EXISTING, 3 UNMONITOR,
# 4 DO_NOTHING; 3.29 adds 5 DELETE_SHOW_IF_EMPTY, 6 UNMONITOR_SHOW_IF_EMPTY, 7 CHANGE_QUALITY_PROFILE.
MT_GROUPS = [
    ("Watched movies", "1", "movie", [mt_rule(0, 5, 0, 0)], 0, 30),       # 1 + a manual member
    ("Unmonitor only", "1", "movie", [mt_rule(0, 5, 1, 1000)], 3, 10),    # 2
    ("Do nothing", "1", "movie", [mt_rule(0, 5, 1, 1000)], 4, 10),        # 3
    ("No deadline", "1", "movie", [mt_rule(0, 5, 1, 1000)], 0, None),     # 4
    ("Inactive", "1", "movie", [mt_rule(0, 5, 0, 0)], 0, 7),              # 5: deactivated, then a manual member
    ("Watched shows", "2", "show", [mt_rule(0, 17, 0, 0)], 1, 14),        # 6
    ("All seasons", "2", "season", [mt_rule(0, 14, 0, 0)], 2, 21),        # 7
    ("Watched episodes", "2", "episode", [mt_rule(0, 5, 0, 0)], 0, 3),    # 8
]
MT_GROUPS_LATEST = MT_GROUPS + [
    ("Tautulli played", "1", "movie", [mt_rule(4, 3, 0, 0)], 0, 5),       # 9: re-run with Tautulli down
    ("Episodes, delete show if empty", "2", "episode", [mt_rule(0, 5, 0, 0)], 5, 5),  # 10
]


def mt_groups(variant):
    return MT_GROUPS_LATEST if variant == "3.29.0" else MT_GROUPS


def mt_wait_rules(variant):
    def idle():
        s = mt(variant, "GET", "/rules/execute/status")
        return not s["processingQueue"] and s["executingRuleGroupId"] is None and not s["pendingRuleGroupIds"]
    time.sleep(3)
    wait_for("the rule run", idle, 300)


def mt_log_collections(variant):
    for c in mt(variant, "GET", "/collections/overlay-data"):
        log("  collection %d %-30s active=%s arrAction=%s deleteAfterDays=%s type=%s media=%s" % (
            c["id"], c["title"], c["isActive"], c["arrAction"], c["deleteAfterDays"], c["type"],
            [(x["mediaServerId"], x["isManual"], x.get("ruleEvaluationFailed")) for x in c["media"]]))


def phase_maintainerr():
    """argv: variant. Builds the collections, members and exclusions described in the index."""
    variant = sys.argv[3]
    ids = load_ids()
    m, sh, se = ids["movies"], ids["shows"], ids["seasons"]
    wait_for("Maintainerr " + variant, lambda: http("GET", mt_base(variant) + "/api/app/status")[0] == 200, 120)
    mt(variant, "POST", "/settings/plex/token", {"plex_auth_token": PLEX_TOKEN})
    mt(variant, "PATCH", "/settings", {"media_server_type": "plex", "plex_name": "bunkarr-fixture-plex",
                                       "plex_hostname": IP["shim"], "plex_port": 32400, "plex_ssl": 0,
                                       # No scheduled rule or collection run during the recording.
                                       "collection_handler_job_cron": "0 0 1 1 *", "rules_handler_job_cron": "0 0 1 1 *"})
    if variant == "3.29.0":
        mt(variant, "POST", "/settings/tautulli", {"url": "http://%s:8181" % IP["tautulli"], "api_key": TAUTULLI_KEY})
    for g in mt_groups(variant):
        mt(variant, "POST", "/rules", mt_group(*g))
    mt(variant, "POST", "/rules/execute")
    mt_wait_rules(variant)
    # Collection 5 is deactivated (which empties it) and then given a manual member.
    mt(variant, "GET", "/collections/deactivate/5")
    mt(variant, "POST", "/collections/add", {"collectionId": 5, "media": [{"mediaServerId": m["His Girl Friday"]}], "manual": True})
    # A manual member of collection 1: Night of the Living Dead was never watched.
    mt(variant, "POST", "/collections/add", {"collectionId": 1, "media": [{"mediaServerId": m["Night of the Living Dead"]}], "manual": True})
    # Exclusions, added after the run so that the excluded items are still members.
    mt(variant, "POST", "/rules/exclusion", {"mediaId": m["The General"]})  # global, a movie
    mt(variant, "POST", "/rules/exclusion", {"collectionId": 6, "mediaId": sh["Alfred Hitchcock Presents"],
                                             "context": {"type": "show", "id": sh["Alfred Hitchcock Presents"]}})
    mt(variant, "POST", "/rules/exclusion", {"collectionId": 7, "mediaId": sh["The Twilight Zone"],
                                             "context": {"type": "show", "id": sh["The Twilight Zone"]}})
    mt(variant, "POST", "/rules/exclusion", {"collectionId": 7, "mediaId": se["Alfred Hitchcock Presents S02"],
                                             "context": {"type": "season", "id": se["Alfred Hitchcock Presents S02"]}})
    mt(variant, "POST", "/rules/exclusion", {"collectionId": 8, "mediaId": se["The Twilight Zone S01"],
                                             "context": {"type": "season", "id": se["The Twilight Zone S01"]}})
    mt_log_collections(variant)


def phase_maintainerr_transient():
    """3.29.0 with Tautulli stopped: rule group 9 is run again; its members stay, flagged
    ruleEvaluationFailed."""
    mt("3.29.0", "POST", "/rules/9/execute")
    mt_wait_rules("3.29.0")
    c = [c for c in mt("3.29.0", "GET", "/collections/overlay-data") if c["id"] == 9][0]
    if not c["media"] or not all(x["ruleEvaluationFailed"] for x in c["media"]):
        raise SystemExit("collection 9 members not flagged: %s" % c["media"])
    mt_log_collections("3.29.0")


def phase_record_maintainerr():
    """argv: variant."""
    variant = sys.argv[3]
    v = MT[variant]
    r = Recorder("maintainerr", v["sub"])
    h = {"Accept": "application/json"}

    def rec(name, path, query="", note=""):
        return r.record(name, "GET", mt_base(variant), path, query, h, note=note, image=v["image"])
    rec("app-status", "/api/app/status", note="JSON served as text/html")
    rec("collections-overlay-data", "/api/collections/overlay-data")
    rec("collections", "/api/collections")
    n = len(mt_groups(variant))
    for cid in range(1, n + 1):
        rec("collections-media-collectionId-%d" % cid, "/api/collections/media/", "collectionId=%d" % cid)
    rec("rules", "/api/rules")
    for gid in range(1, n + 1):
        rec("rules-exclusion-rulegroupId-%d" % gid, "/api/rules/exclusion", "rulegroupId=%d" % gid,
            note=("3.4.1 answers the group's rows plus every exclusion of every group (TypeORM drops the "
                  "ruleGroupId: null condition), with duplicates") if variant == "3.4.1" else "")
    rec("error-collections-media-no-id", "/api/collections/media/", note="collectionId missing")


def phase_maintainerr_handle():
    """argv: variant, zero. Runs Maintainerr's own collection handler (POST /api/collections/handle);
    with zero=1 every collection's deleteAfterDays that is set becomes 0 first, so every member is
    due now. The host waits for the handler's last log line and saves the log excerpt."""
    variant, zero = sys.argv[3], sys.argv[4] == "1"
    if zero:
        for c in mt(variant, "GET", "/collections"):
            if c["deleteAfterDays"] is not None:
                full = mt(variant, "GET", "/collections/collection/%d" % c["id"])
                body = {k: val for k, val in full.items() if k not in ("media", "collectionMedia", "mediaCount")}
                body["deleteAfterDays"] = 0
                mt(variant, "PUT", "/collections", body)
    mt(variant, "POST", "/collections/handle")


def phase_record_maintainerr_after_handle():
    """argv: variant, label."""
    variant, label = sys.argv[3], sys.argv[4]
    v = MT[variant]
    r = Recorder("maintainerr", os.path.join(v["sub"], "probes"))
    notes = {"handle": "after POST /api/collections/handle with the recorded windows; the handler's log is "
                       "handle.log",
             "handle-zero": "after every set deleteAfterDays was changed to 0 and POST /api/collections/handle; the "
                            "handler's log is handle-zero.log"}
    r.record("collections-overlay-data-after-" + label, "GET", mt_base(variant), "/api/collections/overlay-data", "",
             {"Accept": "application/json"}, note=notes[label], image=v["image"])


def phase_maintainerr_global_exclusion():
    """argv: variant. A global exclusion of a show: 3.4.1 materializes the show and every season and
    episode and rewrites those items' group exclusions into global ones (ruleGroupId null)."""
    variant = sys.argv[3]
    v = MT[variant]
    ids = load_ids()
    mt(variant, "POST", "/rules/exclusion", {"mediaId": ids["shows"]["The Twilight Zone"]})
    Recorder("maintainerr", os.path.join(v["sub"], "probes")).record(
        "rules-exclusion-rulegroupId-8-after-global-show-exclusion", "GET", mt_base(variant), "/api/rules/exclusion",
        "rulegroupId=8", {"Accept": "application/json"}, note="after a global exclusion of show The Twilight Zone",
        image=v["image"])


def phase_record_maintainerr_gates():
    r = Recorder("maintainerr")
    for key, image, ver in (("maintainerr-min", MAINTAINERR_MIN_IMAGE, "3.4.0"), ("maintainerr-old", MAINTAINERR_OLD_IMAGE, "3.3.0")):
        base = "http://%s:6246" % IP[key]
        wait_for("Maintainerr " + ver, lambda: http("GET", base + "/api/app/status")[0] == 200, 120)
        r.record("app-status-v" + ver, "GET", base, "/api/app/status", "", {"Accept": "application/json"},
                 image=image, note="an unconfigured instance")
        r.record("collections-overlay-data-v" + ver, "GET", base, "/api/collections/overlay-data", "",
                 {"Accept": "application/json"}, image=image, note="an unconfigured instance")


PHASES = {
    "media": phase_media, "plex": phase_plex, "tautulli-config": phase_tautulli_config, "plays": phase_plays,
    "record-tautulli": phase_record_tautulli, "record-tautulli-disabled": phase_record_tautulli_disabled,
    "record-tautulli-old": phase_record_tautulli_old, "record-plex": phase_record_plex,
    "seerr-seed": phase_seerr_seed, "seerr": phase_seerr, "record-seerr": phase_record_seerr,
    "record-seerr-old": phase_record_seerr_old, "maintainerr": phase_maintainerr,
    "maintainerr-transient": phase_maintainerr_transient, "record-maintainerr": phase_record_maintainerr,
    "maintainerr-handle": phase_maintainerr_handle,
    "record-maintainerr-after-handle": phase_record_maintainerr_after_handle,
    "maintainerr-global-exclusion": phase_maintainerr_global_exclusion,
    "record-maintainerr-gates": phase_record_maintainerr_gates,
}


# ---------------------------------------------------------------------------------------------
# Host orchestration.

def sh(*args, check=True):
    r = subprocess.run(args, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise SystemExit("%s: %s" % (" ".join(args), r.stderr.strip() or r.stdout.strip()))
    return r.stdout.strip()


def runner(phase, *args, volumes=()):
    """Runs one phase of this script in a container on the scene's network."""
    vol = []
    for v in (HERE + ":/rec:ro", TESTDATA + ":/out", PREFIX + "work:/work") + tuple(volumes):
        vol += ["-v", v]
    # Named with the prefix so that teardown also removes a runner left by an interrupted run.
    cmd = ["docker", "run", "--rm", "--name", PREFIX + "run-" + phase, "--network", NET] + vol + [
        RUNNER_IMAGE, "python3", "-u", "/rec/record_slice9.py", "phase", phase] + list(args)
    log("== phase", phase, *args)
    r = subprocess.run(cmd)
    if r.returncode != 0:
        raise SystemExit("phase %s failed" % phase)


def container(name, image, ip, *args, cmd=()):
    sh("docker", "run", "-d", "--name", PREFIX + name, "--network", NET, "--ip", ip, "-e", "TZ=UTC", *args, image, *cmd)


def log_count(name, needle):
    r = subprocess.run(["docker", "logs", PREFIX + name], capture_output=True, text=True)
    return (r.stdout + r.stderr).count(needle)


def wait_logs(name, needle, after=0, timeout=180):
    """Waits until the container's log holds needle more than after times."""
    deadline = time.time() + timeout
    while log_count(name, needle) <= after:
        if time.time() > deadline:
            raise SystemExit("%s: no new %r in the logs" % (name, needle))
        time.sleep(1)


def container_logs(name):
    r = subprocess.run(["docker", "logs", PREFIX + name], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    return r.stdout


LOG_PREFIX = re.compile(r"^\[maintainerr\] \| \d\d/\d\d/\d{4} \d\d:\d\d:\d\d\s+")


def maintainerr_handle(variant, label, zero):
    """Runs Maintainerr's collection handler (see phase_maintainerr_handle), saves the handler's log
    lines (timestamps removed; Maintainerr masks the addresses itself) and records overlay-data."""
    v = MT[variant]
    marker = "Collection size cache updated"
    before = len(container_logs(v["container"]))
    n = log_count(v["container"], marker)
    runner("maintainerr-handle", variant, "1" if zero else "0")
    wait_logs(v["container"], marker, after=n, timeout=300)
    new = container_logs(v["container"])[before:]
    lines = [LOG_PREFIX.sub("", ln) for ln in new.splitlines() if LOG_PREFIX.match(ln) and "[DEBUG]" not in ln]
    start = max(i for i, ln in enumerate(lines) if "Started handling of all collections" in ln)
    end = max(i for i, ln in enumerate(lines) if marker in ln)
    note = ("the log of POST /api/collections/handle with the recorded windows" if not zero else
            "the log of POST /api/collections/handle after every set deleteAfterDays was changed to 0 (PUT "
            "/api/collections)")
    note += "; the lines name the members Maintainerr acted on, whatever PMS answered to the deletion"
    Recorder("maintainerr", os.path.join(v["sub"], "probes"), root=TESTDATA).save(
        label + ".log", "\n".join(lines[start:end + 1]) + "\n",
        {"method": "POST", "path": "/api/collections/handle", "query": "", "auth": "none", "status": None,
         "contentType": "text/plain (container log excerpt)", "image": v["image"], "synthetic": False,
         "sanitized": ["timestamps removed", "DEBUG lines dropped"], "note": note})
    runner("record-maintainerr-after-handle", variant, label)


def plex_up(name, timeout=180):
    deadline = time.time() + timeout
    while subprocess.run(["docker", "exec", PREFIX + name, "curl", "-sf", "-m", "5", "http://127.0.0.1:32400/identity"],
                         capture_output=True).returncode != 0:
        if time.time() > deadline:
            raise SystemExit("PMS did not answer /identity")
        time.sleep(1)


def teardown():
    names = [n for n in sh("docker", "ps", "-a", "--format", "{{.Names}}").splitlines() if n.startswith(PREFIX)]
    if names:
        sh("docker", "rm", "-f", *names, check=False)
    vols = [v for v in sh("docker", "volume", "ls", "--format", "{{.Name}}").splitlines() if v.startswith(PREFIX)]
    if vols:
        sh("docker", "volume", "rm", *vols, check=False)
    sh("docker", "network", "rm", NET, check=False)


def clean_outputs():
    """Removes the files a previous run wrote (as listed in each index.json)."""
    for app in APPS:
        d = os.path.join(TESTDATA, app)
        idx = os.path.join(d, "index.json")
        if os.path.exists(idx):
            with open(idx) as f:
                for e in json.load(f)["files"]:
                    p = os.path.join(d, e["file"])
                    if os.path.exists(p):
                        os.remove(p)
            os.remove(idx)
    for sub in ("tautulli/plex", "maintainerr/probes", "maintainerr/v3.29.0/probes", "maintainerr/v3.29.0"):
        d = os.path.join(TESTDATA, sub)
        if os.path.isdir(d) and not os.listdir(d):
            os.rmdir(d)


def main():
    for image in (PLEX_IMAGE, TAUTULLI_IMAGE, TAUTULLI_OLD_IMAGE, SEERR_IMAGE, SEERR_OLD_IMAGE, MAINTAINERR_IMAGE,
                  MAINTAINERR_LATEST_IMAGE, MAINTAINERR_MIN_IMAGE, MAINTAINERR_OLD_IMAGE, RUNNER_IMAGE):
        if subprocess.run(["docker", "image", "inspect", image], capture_output=True).returncode != 0:
            log("pulling", image)
            sh("docker", "pull", "-q", image)
    teardown()
    clean_outputs()
    try:
        record()
    finally:
        teardown()
    problems = check()
    for p in problems:
        log("check:", p)
    if problems:
        raise SystemExit("the recorded files fail the check")
    log("done; check passed")


def record():
    sh("docker", "network", "create", "--subnet", SUBNET, NET)
    for v in ("work", "media", "plex", "tautulli", "tautulli-old", "seerr", "seerr-old", "maintainerr",
              "maintainerr-latest", "maintainerr-min", "maintainerr-old"):
        sh("docker", "volume", "create", PREFIX + v)
    runner("media", volumes=(PREFIX + "media:/media",))

    # PMS: the first start writes Preferences.xml; allowedNetworks is added while it is stopped.
    container("plex", PLEX_IMAGE, IP["plex"], "-e", "PLEX_UID=1000", "-e", "PLEX_GID=1000",
              "-v", PREFIX + "plex:/config", "-v", PREFIX + "media:/data:ro")
    plex_up("plex")
    sh("docker", "stop", "-t", "60", PREFIX + "plex")
    sh("docker", "run", "--rm", "--user", "1000:1000", "-v", PREFIX + "plex:/config", "-e", "ALLOWED=" + PLEX_ALLOWED,
       "--entrypoint", "bash", PLEX_IMAGE, "-c",
       'set -euo pipefail; P="/config/Library/Application Support/Plex Media Server/Preferences.xml"; '
       'grep -q "<Preferences " "$P"; sed -i "s|/>\\$| allowedNetworks=\\"$ALLOWED\\"/>|" "$P"; '
       'grep -q "allowedNetworks=\\"$ALLOWED\\"" "$P"')
    sh("docker", "start", PREFIX + "plex")
    plex_up("plex")
    runner("plex")
    container("shim", RUNNER_IMAGE, IP["shim"], "-v", HERE + ":/rec:ro", cmd=("python3", "-u", "/rec/record_slice9.py", "shim"))

    # Tautulli 2.18.1: the first start writes config.ini, which is then edited while stopped.
    container("tautulli", TAUTULLI_IMAGE, IP["tautulli"], "-e", "PUID=1000", "-e", "PGID=1000",
              "-v", PREFIX + "tautulli:/config")
    wait_logs("tautulli", "Tautulli is ready!")
    sh("docker", "stop", "-t", "30", PREFIX + "tautulli")
    runner("tautulli-config", "1", volumes=(PREFIX + "tautulli:/c",))
    ws = log_count("tautulli", "Tautulli WebSocket :: Ready")
    sh("docker", "start", PREFIX + "tautulli")
    wait_logs("tautulli", "Tautulli WebSocket :: Ready", after=ws)
    runner("plays")
    runner("record-tautulli")
    runner("record-plex")

    # Seerr 3.4.1: the first start creates the database, which is seeded while stopped.
    container("seerr", SEERR_IMAGE, IP["seerr"], "-e", "LOG_LEVEL=debug", "-v", PREFIX + "seerr:/app/config")
    wait_logs("seerr", "Server ready on port 5055")
    sh("docker", "stop", "-t", "20", PREFIX + "seerr")
    runner("seerr-seed", volumes=(PREFIX + "seerr:/c",))
    sh("docker", "start", PREFIX + "seerr")
    runner("seerr")
    runner("record-seerr")

    # Maintainerr 3.4.1 (the contract's version), through the shim.
    container("maintainerr", MAINTAINERR_IMAGE, IP["maintainerr"], "-v", PREFIX + "maintainerr:/opt/data")
    runner("maintainerr", "3.4.1")
    runner("record-maintainerr", "3.4.1")

    # Maintainerr 3.29.0 (the latest release) with the same collections plus two: a Tautulli rule
    # that is run again while Tautulli is stopped (its members get ruleEvaluationFailed), and
    # ServarrAction 5.
    container("maintainerr-latest", MAINTAINERR_LATEST_IMAGE, IP["maintainerr-latest"],
              "-v", PREFIX + "maintainerr-latest:/opt/data")
    runner("maintainerr", "3.29.0")
    sh("docker", "stop", "-t", "30", PREFIX + "tautulli")
    runner("maintainerr-transient")
    runner("record-maintainerr", "3.29.0")

    # The probes change PMS (Maintainerr's handler deletes through it) and the exclusions, so they
    # come after both main sets.
    maintainerr_handle("3.4.1", "handle", zero=False)
    maintainerr_handle("3.4.1", "handle-zero", zero=True)
    runner("maintainerr-global-exclusion", "3.4.1")
    maintainerr_handle("3.29.0", "handle-zero", zero=True)
    runner("maintainerr-global-exclusion", "3.29.0")

    # Tautulli with the API disabled (it is stopped at this point).
    runner("tautulli-config", "0", volumes=(PREFIX + "tautulli:/c",))
    ready = log_count("tautulli", "Tautulli is ready!")
    sh("docker", "start", PREFIX + "tautulli")
    wait_logs("tautulli", "Tautulli is ready!", after=ready)
    runner("record-tautulli-disabled")

    # Version gates: Tautulli 2.17.2 (configured like 2.18.1), Seerr 3.3.0, Maintainerr 3.4.0 and 3.3.0.
    container("tautulli-old", TAUTULLI_OLD_IMAGE, IP["tautulli-old"], "-e", "PUID=1000", "-e", "PGID=1000",
              "-v", PREFIX + "tautulli-old:/config")
    wait_logs("tautulli-old", "Tautulli is ready!")
    sh("docker", "stop", "-t", "30", PREFIX + "tautulli-old")
    runner("tautulli-config", "1", volumes=(PREFIX + "tautulli-old:/c",))
    ready = log_count("tautulli-old", "Tautulli is ready!")
    sh("docker", "start", PREFIX + "tautulli-old")
    wait_logs("tautulli-old", "Tautulli is ready!", after=ready)
    runner("record-tautulli-old")
    container("seerr-old", SEERR_OLD_IMAGE, IP["seerr-old"], "-v", PREFIX + "seerr-old:/app/config")
    runner("record-seerr-old")
    container("maintainerr-min", MAINTAINERR_MIN_IMAGE, IP["maintainerr-min"], "-v", PREFIX + "maintainerr-min:/opt/data")
    container("maintainerr-old", MAINTAINERR_OLD_IMAGE, IP["maintainerr-old"], "-v", PREFIX + "maintainerr-old:/opt/data")
    runner("record-maintainerr-gates")

    recorded = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    common = {"recorder": "testdata/tautulli/record_slice9.py", "recordedAt": recorded, "plex": PLEX_IMAGE}
    meta = {
        "tautulli": {"app": "Tautulli", "image": TAUTULLI_IMAGE, "gateImage": TAUTULLI_OLD_IMAGE,
                     "base": "http://<tautulli>:8181", "key": "X-Api-Key header (fake key of the throw-away container)"},
        "seerr": {"app": "Seerr", "image": SEERR_IMAGE, "gateImage": SEERR_OLD_IMAGE,
                  "base": "http://<seerr>:5055", "key": "X-Api-Key header (fake key of the throw-away container)"},
        "maintainerr": {"app": "Maintainerr", "image": MAINTAINERR_IMAGE, "latestImage": MAINTAINERR_LATEST_IMAGE,
                        "gateImages": [MAINTAINERR_MIN_IMAGE, MAINTAINERR_OLD_IMAGE],
                        "base": "http://<maintainerr>:6246", "key": "none (Maintainerr has no API key)"},
    }
    for app in APPS:
        m = dict(common, **meta[app])
        m["scenario"] = SCENARIO[app]
        ids = subprocess.run(["docker", "run", "--rm", "-v", PREFIX + "work:/work", RUNNER_IMAGE, "cat", "/work/plex-ids.json"],
                             capture_output=True, text=True).stdout
        m["plexIds"] = json.loads(ids)
        _write_meta(app, m)


def _write_meta(app, meta):
    path = os.path.join(TESTDATA, app, "index.json")
    with open(path) as f:
        idx = json.load(f)
    files = idx.pop("files")
    idx.update(meta)
    idx["files"] = files
    with open(path, "w") as f:
        json.dump(idx, f, indent=2, ensure_ascii=False)
        f.write("\n")


# ---------------------------------------------------------------------------------------------
# Offline check of the recorded files (no Docker): python3 testdata/tautulli/record_slice9.py check

def check(root=TESTDATA):
    """Verifies the index against the files, that no secret is recorded, that the paging samples
    concatenate to the full answers, and that every rating key, guid and tmdb id the three apps
    recorded joins the recorded PMS listings. Returns the list of problems (empty when all hold)."""
    problems = []

    def load(app, rel):
        with open(os.path.join(root, app, rel), encoding="utf-8") as f:
            return json.load(f)

    # Index <-> files, secrets, JSON validity.
    for app in APPS:
        d = os.path.join(root, app)
        with open(os.path.join(d, "index.json"), encoding="utf-8") as f:
            idx = json.load(f)
        listed = {e["file"] for e in idx["files"]}
        on_disk = set()
        for base, _, files in os.walk(d):
            for fn in files:
                rel = os.path.relpath(os.path.join(base, fn), d)
                if rel not in ("index.json", "record_slice9.py") and not rel.endswith(".pyc"):
                    on_disk.add(rel)
        for rel in sorted(listed - on_disk):
            problems.append("%s: %s is indexed but missing" % (app, rel))
        for rel in sorted(on_disk - listed):
            problems.append("%s: %s is not indexed" % (app, rel))
        for rel in sorted(listed & on_disk):
            with open(os.path.join(d, rel), encoding="utf-8") as f:
                text = f.read()
            for s in SECRETS:
                if s in text:
                    problems.append("%s: %s holds a secret" % (app, rel))
            if rel.endswith(".json"):
                try:
                    json.loads(text)
                except ValueError as e:
                    problems.append("%s: %s: %s" % (app, rel, e))
        if any(e.get("synthetic") for e in idx["files"]):
            problems.append("%s: a file is marked synthetic" % app)

    # PMS items: rating key -> (guid, tmdb id, parent, grandparent).
    plex = {}
    for fn in os.listdir(os.path.join(root, "tautulli", "plex")):
        if re.match(r"^library-sections-\d+-all-type-\d+\.json$", fn):
            for m in load("tautulli", "plex/" + fn)["MediaContainer"].get("Metadata", []):
                tmdb = [g["id"][7:] for g in m.get("Guid", []) if g["id"].startswith("tmdb://")]
                plex[m["ratingKey"]] = {"guid": m["guid"], "tmdb": tmdb[0] if tmdb else None,
                                        "parent": m.get("parentRatingKey"), "grandparent": m.get("grandparentRatingKey")}

    def show_tmdb(rk):
        it = plex[rk]
        top = it["grandparent"] or it["parent"] or rk
        return plex.get(top, it)["tmdb"] if top in plex else it["tmdb"]

    machine = load("tautulli", "plex/identity.json")["MediaContainer"]["machineIdentifier"]

    # Tautulli.
    if load("tautulli", "get_server_info.json")["response"]["data"]["pms_identifier"] != machine:
        problems.append("tautulli: pms_identifier differs from the PMS machineIdentifier")
    for sec in ("1", "2", "3"):
        rows = load("tautulli", "get_history-section_id-%s.json" % sec)["response"]["data"]["data"]
        for r in rows:
            for k in ("rating_key", "parent_rating_key", "grandparent_rating_key"):
                if r[k] != "" and str(r[k]) not in plex:
                    problems.append("tautulli: row %s %s %s is not in PMS" % (r["row_id"], k, r[k]))
            if str(r["rating_key"]) in plex and plex[str(r["rating_key"])]["guid"] != r["guid"]:
                problems.append("tautulli: row %s guid %s differs from PMS" % (r["row_id"], r["guid"]))
    full = load("tautulli", "get_history-section_id-1.json")["response"]["data"]["data"]
    paged = []
    for start in (0, 2, 4, 6):
        paged += load("tautulli", "get_history-section_id-1-length-2-start-%d.json" % start)["response"]["data"]["data"]
    if [r["row_id"] for r in paged] != [r["row_id"] for r in full]:
        problems.append("tautulli: the history pages do not concatenate to the full answer")

    # Seerr.
    reqs = load("seerr", "request-take-100-skip-0.json")["results"]
    for r in reqs:
        m = r["media"]
        if m.get("ratingKey") is not None:
            rk = m["ratingKey"]
            if rk not in plex:
                problems.append("seerr: request %d ratingKey %s is not in PMS" % (r["id"], rk))
            elif str(show_tmdb(rk)) != str(m["tmdbId"]):
                problems.append("seerr: request %d tmdb %s differs from PMS" % (r["id"], m["tmdbId"]))
    paged = []
    for skip in (0, 4, 8):
        paged += load("seerr", "request-take-4-skip-%d.json" % skip)["results"]
    if [r["id"] for r in paged] != [r["id"] for r in reqs]:
        problems.append("seerr: the request pages do not concatenate to the full answer")
    users = load("seerr", "user-take-100-skip-0.json")["results"]
    paged = load("seerr", "user-take-2-skip-0.json")["results"] + load("seerr", "user-take-2-skip-2.json")["results"]
    if sorted(u["id"] for u in paged) != sorted(u["id"] for u in users):
        problems.append("seerr: the user pages do not cover the full answer")

    # Maintainerr, both variants.
    for sub in ("", "v3.29.0/"):
        for c in load("maintainerr", sub + "collections-overlay-data.json"):
            for m in c["media"]:
                rk = m["mediaServerId"]
                if rk not in plex:
                    problems.append("maintainerr %s: collection %d member %s is not in PMS" % (sub or "3.4.1", c["id"], rk))
                elif str(show_tmdb(rk)) != str(m["tmdbId"]):
                    problems.append("maintainerr %s: member %s tmdb %s differs from PMS" % (sub or "3.4.1", rk, m["tmdbId"]))
            per = load("maintainerr", sub + "collections-media-collectionId-%d.json" % c["id"])
            if sorted(x["id"] for x in per) != sorted(x["id"] for x in c["media"]):
                problems.append("maintainerr %s: collection %d media differ from overlay-data" % (sub or "3.4.1", c["id"]))

    # PMS paging sample.
    eps = load("tautulli", "plex/library-sections-2-all-type-4.json")["MediaContainer"]["Metadata"]
    paged = []
    for start in (0, 4, 8):
        paged += load("tautulli", "plex/library-sections-2-all-type-4-start-%d-size-4.json" % start)["MediaContainer"]["Metadata"]
    if [m["ratingKey"] for m in paged] != [m["ratingKey"] for m in eps]:
        problems.append("plex: the episode pages do not concatenate to the full listing")
    return problems


# ---------------------------------------------------------------------------------------------
# The PMS shim (see the module comment): forwards everything except POST /hubs/sections/<id>/manage.

def shim():
    import http.server
    manage = re.compile(r"^/hubs/sections/(\d+)/manage$")

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def _send(self, code, headers, body):
            self.send_response(code)
            for k, v in headers:
                if k.lower() not in ("transfer-encoding", "connection", "content-length"):
                    self.send_header(k, v)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _do(self):
            u = urllib.parse.urlsplit(self.path)
            n = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(n) if n else None
            mm = manage.match(u.path)
            if self.command == "POST" and mm:
                q = urllib.parse.parse_qs(u.query)
                hub = {"identifier": "custom.collection.%s.%s" % (mm.group(1), q.get("metadataItemId", ["0"])[0])}
                for k in ("promotedToRecommended", "promotedToOwnHome", "promotedToSharedHome"):
                    hub[k] = int(q.get(k, ["0"])[0])
                log("shim: answered", self.command, u.path)
                return self._send(200, [("Content-Type", "application/json")],
                                  json.dumps({"MediaContainer": {"size": 1, "Hub": [hub]}}).encode())
            hdrs = {k: v for k, v in self.headers.items()
                    if k.lower() not in ("host", "connection", "content-length", "accept-encoding")}
            req = urllib.request.Request(PLEX + self.path, data=body, method=self.command, headers=hdrs)
            try:
                with urllib.request.urlopen(req, timeout=120) as r:
                    return self._send(r.status, r.getheaders(), r.read())
            except urllib.error.HTTPError as e:
                return self._send(e.code, list(e.headers.items()), e.read())

        do_GET = do_POST = do_PUT = do_DELETE = _do

        def log_message(self, *a):
            pass

    http.server.ThreadingHTTPServer(("0.0.0.0", 32400), Handler).serve_forever()


if __name__ == "__main__":
    if len(sys.argv) > 2 and sys.argv[1] == "phase":
        PHASES[sys.argv[2]]()
    elif sys.argv[1:] == ["shim"]:
        shim()
    elif sys.argv[1:] == ["check"]:
        found = check()
        for p in found:
            print("check:", p)
        print("check: %d problems" % len(found))
        sys.exit(1 if found else 0)
    elif sys.argv[1:] == ["record"]:
        main()
    else:
        sys.exit("usage: record_slice9.py record|check (see the module comment)")
