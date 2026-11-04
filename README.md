# Homelab: a Community App Store for umbrelOS

A personal store with three apps:

| App | What it does |
|---|---|
| [Disk Live Usage](#disk-live-usage) (`homelab-disk-live-usage`) | Live Usage for disks: who reads and writes to the disk |
| [Floor Heating](#floor-heating) (`homelab-floor-heating`) | History for Tuya Wi-Fi floor heating thermostats |
| [Kinoseed](#kinoseed) (`homelab-kinoseed`) | A qBittorrent web UI built around movies and TV series |

To add the store to umbrelOS, see [Install in umbrelOS](#install-in-umbrelos).

## Disk Live Usage

Like umbrelOS's built-in Live Usage (CPU and RAM per app), but for disks.
It shows in real time (every second, over SSE) **who exactly** is reading and writing to the disk:

| Block | Source | What you see |
|---|---|---|
| Disks | `/proc/diskstats` | read/write MB/s, IOPS, utilisation % (io_ticks) |
| Apps | cgroup v2 `io.stat` + Docker API | MB/s per Umbrel app (containers grouped by `<app-id>_<service>_1`) |
| Host services | `system.slice/*.service/io.stat` | `umbrel` (umbreld), `docker`, `containerd`… only while they are doing something |
| Processes | `/proc/<pid>/io` | top 15 host processes, labelled with their app or systemd unit |
| Summary | `/proc/stat`, `/proc/vmstat`, `/proc/meminfo` | CPU iowait, swap in/out, swap used, memory available |

The last block is why this exists: on a Raspberry Pi the biggest disk consumer turned out to be
`umbreld` (the umbrelOS file indexer, `node /opt/umbreld`), which is not a container, so `docker stats` doesn't show it.

![screenshot](docs/screenshot.jpg)

## Layout

```
umbrel-app-store.yml              id: homelab, the prefix for every app in this store
homelab-kinoseed/
  umbrel-app.yml                  Kinoseed manifest (port 8795 on Umbrel)
  docker-compose.yml              app_proxy + web service (image built in the Kinoseed repository)
  icon.svg                        app icon
  data/                           copied to the app's data folder, where Kinoseed keeps its settings
homelab-disk-live-usage/
  umbrel-app.yml                  manifest (port 7841 on Umbrel)
  docker-compose.yml              app_proxy + web service
  Dockerfile                      Go → static binary in scratch (~8–11 MB image)
  *.go, web/index.html            collector and UI (Go standard library only, no JS dependencies)
homelab-floor-heating/
  umbrel-app.yml                  manifest (port 7842 on Umbrel)
  docker-compose.yml              app_proxy + web service + neighbors service
  Dockerfile                      Go → static binary in scratch
  *.go, web/index.html            poller and UI (Go standard library only, no JS dependencies)
  data/                           copied to the app's data folder: device list and history
dev/docker-compose.yml            local run of Disk Live Usage without umbrelOS
.github/workflows/docker.yml      builds the Disk Live Usage image into GHCR on a v* tag
.github/workflows/docker-floor-heating.yml
                                  builds the Floor Heating image on a floor-heating-v* tag
```

The collector is light: one pass over `/proc` and cgroupfs per tick, no external commands, and the container list
from the Docker API refreshes every 5 s. **It only polls while the page is open** (or something calls
`/api/snapshot`): close the tab and the app does nothing. On a Mac (Docker Desktop) under load: ~0.1 % CPU, 6.4 MiB RAM;
not measured on a Pi yet, expected around 1–2 % of one core.

## ⚠️ Security: why this is for a personal store, not the official one

To see host processes and map cgroups to apps, the container runs with:

- `pid: host`: it sees every Umbrel process and its command line (which can contain passwords or tokens in arguments);
- `cap_add: SYS_PTRACE, DAC_READ_SEARCH` and `apparmor:unconfined`: needed for the kernel to allow reading `/proc/<pid>/io`
  of other users' root processes (umbreld). Together with `pid: host` this makes it technically possible to attach (ptrace) to any host process;
- `/var/run/docker.sock:ro`: **`:ro` on the socket protects nothing**. The socket lets you not only read but also start
  containers with any privileges. The app makes exactly one request, `GET /containers/json`, but socket access itself means root on the host.

In short: **the app is equivalent to root access to your Umbrel**. Mitigations: `cap_drop: ALL` (only the two needed
capabilities are added back), `read_only: true`, `no-new-privileges`, `mem_limit: 64m`, a `scratch` image without a shell, no outgoing
connections, host paths mounted read-only and nothing written. The UI is behind Umbrel's login through `app_proxy`.
The code is short (~800 lines of Go), so you can read all of it before installing. The official Umbrel App Store would most
likely not accept an app like this; it is meant for a personal community store.

To reduce the risk, run [docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) next to it with `CONTAINERS=1`
and set `DOCKER_HOST=tcp://homelab-disk-live-usage_docker_proxy_1:2375`: the service can reach Docker over TCP too.
Without the socket at all, only app names disappear (disks and processes stay).

### Publishing the image

Image: `ghcr.io/mrshakhov/umbrel-disk-live-usage` (linux/arm64 + linux/amd64). GitHub Actions
([.github/workflows/docker.yml](.github/workflows/docker.yml)) builds it when a `v*` tag is pushed:

```bash
git tag v0.1.1 && git push origin v0.1.1   # → ghcr.io/mrshakhov/umbrel-disk-live-usage:0.1.1
```

New release: bump `version` in `umbrel-app.yml` and the image tag in `docker-compose.yml`, commit, push,
and push a tag with the same number. The GHCR package must be **public**
(GitHub → Packages → umbrel-disk-live-usage → Package settings → Change visibility → Public), or Umbrel can't pull the image.

### Running locally (Mac/Linux with Docker)

```bash
docker compose -f dev/docker-compose.yml up --build
```

UI: <http://localhost:8080>, JSON: `curl localhost:8080/api/snapshot`.

On a Mac it works but shows **the Docker Desktop virtual machine**, not the Mac itself: disks `vda/vdb`,
the VM's `dockerd`/`containerd` processes, Docker Desktop's containers. There are no systemd services, so no "host" rows.

### Reading the numbers, known limits

- MB = 10⁶ bytes. 100 % utilisation on an HDD means the queue isn't draining.
- `/proc/<pid>/io` counts real disk access: reads from the page cache aren't counted, and writes are counted when
  the page is dirtied, so a process's write peak can run a few seconds ahead of the disk (writeback).
- The kernel adds the I/O of finished child processes to their parent, so short `dd`/`ffmpeg` runs show up under their parent.
- cgroup `io.stat` also counts deferred writes, so the per-app total is closer to the disk numbers than the per-process total.
- **Not tested on a real Umbrel.** It expects cgroup v2 with Docker's systemd driver (`system.slice/docker-<id>.scope`);
  `docker/<id>` is supported too. If yellow notes appear at the bottom of the page, they say what is missing
  (no io.stat, Docker API unreachable, N processes unreadable because of AppArmor or permissions, and so on).

### Settings (env in docker-compose.yml)

| Variable | Default | |
|---|---|---|
| `INTERVAL` | `1s` | polling interval (at least 250ms) |
| `TOP_PROCS` | `15` | how many processes to show |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | or `tcp://host:port` |
| `CGROUP_ROOT` | `/host/cgroup` | where the host's `/sys/fs/cgroup` is mounted |
| `PROC_ROOT`, `SYS_ROOT` | `/proc`, `/sys` | |
| `LISTEN` | `:8080` | |

## Floor Heating

Tuya Wi-Fi thermostats don't keep the history of room temperature or heating. This app polls them over the LAN
once a minute, records what they report and charts it over 24 hours, 7 days or 30 days: room temperature,
set temperature, when the heating was on, and how many hours it ran.

- **Read-only:** the only command in the code is the status query (`DP_QUERY`), so the app cannot change a thermostat.
- **Protocol:** Tuya local protocol 3.3 on TCP 6668. Other versions aren't supported yet.
- **No cloud at runtime:** the Tuya cloud is needed once, to get the local keys. The only traffic beyond your LAN is
  [Telegram alerts](#telegram-alerts), if you turn them on.
- **Limits:** the poller runs on Docker's bridge network, so it can't hear the devices' UDP broadcasts. Give each thermostat a
  fixed IP in your router's DHCP settings. Connections go one at a time and last a moment, and each device takes only a few at once.
- A thermostat's key changes when it is paired again in the mobile app. The page then shows an error for that device.
  Enter the new key and history continues.
- **Poll status:** a ring in the page header fills up towards the next poll, next to a countdown. It turns red and stops,
  with the word **Stalled**, when the collector misses its schedule or the page can't reach the app.

### Thermostats that stop answering

After a while these thermostats can stop answering ARP requests, while staying on Wi-Fi and in the mobile app. Polls then fail
with `no route to host`, often for one thermostat and only until the router restarts. Anything that already knows the MAC
address still reaches them, so the cause is ARP and not the network. Why the thermostats do this is unknown.

So the app pins each address on the Umbrel. Enter the MAC address under **Devices**: it's in your router's list of clients.
Leave it empty to skip pinning for that thermostat.

The `neighbors` service does this every 30 s, and pins survive restarts because the service sets them again. It's the same image,
with less access than it may look:

- It needs `network_mode: host` and `NET_ADMIN`, which allow changing the Umbrel's network settings. Those are the only extras.
- It runs as root because capabilities don't reach a non-root user, but with `DAC_OVERRIDE` dropped, so it can't open the
  `0600` device list. It reads only `neighbors.json`, which holds IP and MAC addresses and no keys. The folder is read-only.
- If a thermostat's IP or MAC changes, update it under **Devices**. A wrong pinned address makes that thermostat unreachable.

### Telegram alerts

The app can message you when a thermostat stops answering, and again when it's back. A few failed polls in a row
(5 by default) make an outage, so a single lost poll stays quiet. Each outage gets one message, with the error and a hint
for the errors these thermostats are known for:

```text
🔴 Bedroom stopped answering 4 min ago.
dial tcp 192.168.3.126:6668: i/o timeout
If it's still in your router's list of clients, its local server may be stuck: …

🟢 Bedroom answers again after 17 h 41 min.
```

To turn them on, open **Telegram alerts** on the app's page:

1. In Telegram, create a bot with [@BotFather](https://t.me/BotFather) and paste its token.
2. Send any message to your bot, then press **Find chat**. It fills in the chat ID from that message.
3. Save, then press **Send test**.

The token works like the local keys: it stays in `notify.json` in the data folder (mode 0600), the API never returns it,
and **Turn off** deletes it. If a message can't be sent, the app tries again after 30 s, 2 min and 10 min, and the page shows
the error. Outages are counted in memory, so after a restart a thermostat that is still down is reported again.

The app can't report its own failure: if it or the Umbrel stops, no alert goes out.

### Setup

1. Create a project on the [Tuya IoT Platform](https://iot.tuya.com) and link your mobile app account to it.
2. On your computer, run the wizard: `pip install tinytuya && python -m tinytuya wizard`. It writes `devices.json`.
3. Open the app, paste that file into **Import devices.json**, check the IP and MAC addresses and save.

`devices.json` holds your local keys, so don't commit it or share it. In the app, keys stay in the data folder
(`devices.json`, mode 0600), and the API never returns them.

### Data

One [JSON Lines](https://jsonlines.org) file per UTC day in `data/history/`, also available from `/api/history/YYYY-MM-DD`.
Each line is one poll of one device. Failed polls are kept with `err`, so an outage can be told from a gap:

```json
{"t":"2026-11-01T14:00:26Z","id":"bf00…","ms":41,"dps":{"1":true,"16":240,"24":219}}
```

`/api/status` returns the latest state of each device, without keys, and what the collector is doing. Since 0.4.0 it's an object
rather than a bare list. `state` is `polling`, `waiting` or `stalled`. Times are milliseconds from the moment of the response,
so the browser's clock doesn't matter, and `next_in_ms` goes negative once a poll is overdue:

```json
{"poll":{"state":"waiting","next_in_ms":42000,"last_ago_ms":18000},"devices":[{"id":"bf00…","name":"Bath","ip":"192.0.2.10"}]}
```

`/api/notify` returns the alert settings without the token, and the error of the last message if it failed. `PUT` saves
them (an empty `token` keeps the stored one), `DELETE` turns alerts off, `POST /api/notify/test` sends a test message,
and `POST /api/notify/chat` finds the chat that last wrote to the bot.

Data point numbers and scales differ between models, so the history stores them as the device reports them.
`/api/series?range=24h|7d|30d` returns averaged buckets for the charts. Heating time counts polls that found the heating on,
so it's accurate to one poll interval.

### Supported thermostat

The charts read these data points of the `SS210RH_WH_WF` thermostat (see the constants in `series.go`). Other models may need
different numbers.

| DP | Meaning | Reading |
|---|---|---|
| 1 | Power | `true` / `false` |
| 3 | Work state (not in the vendor's spec) | `heating`, or `warming` while idle |
| 16 | Set temperature | tenths of °C |
| 24 | Room temperature | tenths of °C |

Settings (env of the `web` service): `POLL_INTERVAL` (default `60s`, at least `30s`), `DATA_DIR` (`/data`), `LISTEN` (`:8080`).

Image: `ghcr.io/mrshakhov/umbrel-floor-heating` (linux/arm64 + linux/amd64). New release: bump `version` in `umbrel-app.yml`
and the image tag in `docker-compose.yml`, merge, then push a tag such as `floor-heating-v0.1.1`. The GHCR package must be **public**.

Tests need only Docker:

```bash
docker run --rm -v "$PWD/homelab-floor-heating":/src:ro -w /src -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false -e GOCACHE=/tmp/c golang:1.23-alpine go test ./...
```

## Kinoseed

A qBittorrent web UI built around movies and TV series: search TMDB, pick a release found by Prowlarr,
send it to qBittorrent and follow your downloads. Installable on your phone as an app (PWA).

- **Dependencies:** the **qBittorrent** and **Prowlarr** apps from the Umbrel App Store. Umbrel won't install Kinoseed without them.
- **qBittorrent:** Kinoseed finds it on its own and signs in with Umbrel's default login `admin`/`adminadmin`. If the login was changed,
  Kinoseed asks for it once and keeps it.
- **Prowlarr:** the API key is read from Prowlarr's `config.xml`. Its `config` folder is mounted read-only.
- **TMDB:** on first launch Kinoseed asks for a free [TMDB API key](https://www.themoviedb.org/settings/api)
  and keeps it on your Umbrel, in the app's data folder.
- The UI is behind Umbrel's login through `app_proxy`.

Image: `ghcr.io/mrshakhov/kinoseed` (linux/arm64 + linux/amd64), published by the Kinoseed repository's Release workflow
on a `v*` tag. `docker-compose.yml` pins it by tag and digest.

New release: in `homelab-kinoseed/`, bump `version` and `releaseNotes` in `umbrel-app.yml` and the image tag and digest
in `docker-compose.yml` (the digest is in the GitHub Release), then commit and push. Umbrel offers the update.

## Install in umbrelOS

1. Open **App Store** → the **⋯** menu at the top right → **Community App Stores**.
2. Paste the repository URL (`https://github.com/MrShakhov/umbrel-homelab-store`) → **Add**.
3. Open the new **Homelab App Store** → pick an app → **Install**.
4. Disk Live Usage opens at `http://umbrel.local:7841`, Floor Heating at `http://umbrel.local:7842`, Kinoseed at `http://umbrel.local:8795`.

After a new release (see above) Umbrel offers the update on its own.

To rename the store, change `id` in `umbrel-app-store.yml`, the app folder names, `id` in each manifest and `APP_HOST`
in each compose file: all of them must share the prefix.
