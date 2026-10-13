# Beats

Put Beatport downloads behind a clean web interface instead of babysitting a terminal.

Beats lets you search the Beatport catalog, pick tracks and releases, choose where they belong, and follow each download from the queue to your library. Finished tracks are converted to 320 kbps MP3 while keeping the useful details: cover art, title, artist, album, genre, release date, track and disc numbers, label, catalog number, and ISRC.

If you liked the simple workflow of Deemix and want something similar for music available through your Beatport or Beatsource streaming plan, download Beats and give it a run.

> Beats requires an active Beatport Streaming or Beatsource subscription. Please use it only for music you are entitled to access and follow the service terms that apply to your account.

## What you get

- A first-run sign-in screen—no config file editing required
- Beatport catalog search and direct URL support
- A download queue with clear success and failure states
- Automatic fallback to a quality accepted by your subscription
- Automatic 320 kbps MP3 conversion after a successful download
- Preserved tags and embedded album artwork
- A library with cover art, metadata, and an in-browser player
- Named download folders for keeping music organized
- One-click library ZIP creation
- Explicit confirmation before archived originals are deleted
- Docker support with persistent data and download folders

## The quickest way to try it

Docker Compose is the easiest route:

```shell
git clone https://github.com/stevensonmediagroup/beats.git
cd beats
mkdir -p data downloads archive
chmod 700 data
docker compose up -d --build
```

Open [http://localhost:8080](http://localhost:8080), sign in to Beatport on the setup screen, and start searching.

Your files stay on the host:

| Host directory | Contents |
| --- | --- |
| `./data` | BeatportDL configuration and session data |
| `./downloads` | Main music library |
| `./archive` | Optional second destination |

Treat `./data` as private because it contains the saved Beatport session. It is ignored by Git and is not copied into the Docker image.

Useful Docker commands:

```shell
# Check whether the service is healthy
docker compose ps

# Follow the application log
docker compose logs -f beats

# Pull in repository changes and rebuild
git pull
docker compose up -d --build

# Stop the service without deleting your music or configuration
docker compose down
```

The container runs as user and group `1000:1000` by default. If that does not match the account that owns your music directories, set `PUID` and `PGID` before starting it:

```shell
PUID="$(id -u)" PGID="$(id -g)" docker compose up -d --build
```

## How Beats handles a download

1. You search for music or paste a supported Beatport or Beatsource URL.
2. You choose a configured destination folder and add the item to the queue.
3. Beats tries your preferred source quality, then lower supported qualities if the service rejects it.
4. A completed source file is converted to 320 kbps MP3.
5. FFmpeg carries the source metadata and embedded artwork into the MP3.
6. Beats validates that the MP3 exists before reporting the job as complete.
7. Only after successful conversion does it remove the original FLAC, M4A, or AAC file.

The Library page reads metadata from the finished files and shows whatever is available. You can play a track in the browser or package the whole library into a ZIP. Building a ZIP does not delete the music. Deletion happens only after you download the archive and press the confirmation button; Beats refuses the deletion if an archived file changed after the ZIP was built.

## Choose your download folders

The included Compose file exposes two destinations:

```yaml
BEATS_FOLDERS: "Downloads=/downloads,Archive=/archive"
```

To add or rename destinations, edit `BEATS_FOLDERS` in `compose.yaml` and add a matching volume mount. Paths inside the container must be absolute.

For a native installation, use the same variable when starting the server:

```shell
BEATS_FOLDERS="Music=/srv/music,DJ Sets=/srv/sets" ./beats-web
```

## Run it without Docker

You need Go 1.24, FFmpeg, TagLib 2, a C/C++ compiler, and zlib development headers.

Build both programs from the repository root:

```shell
go build -o beatportdl ./cmd/beatportdl
go build -o beats-web ./cmd/beatsweb
./beats-web
```

Then open [http://127.0.0.1:8080](http://127.0.0.1:8080).

Runtime settings:

| Variable | Default | Purpose |
| --- | --- | --- |
| `BEATS_ADDR` | `127.0.0.1:8080` | Address used by the web server |
| `BEATPORTDL_BIN` | `./beatportdl` | Path to the downloader executable |
| `BEATS_DATA_DIR` | `.` | Location of configuration, session data, and the default download folder |
| `BEATS_FOLDERS` | `Downloads=<data-dir>/downloads` | Comma-separated `Name=/absolute/path` destination list |

Equivalent command-line flags are available:

```shell
./beats-web -addr 0.0.0.0:8080 -downloader ./beatportdl -workdir ./data
```

## Supported links

The underlying downloader accepts Beatport and Beatsource tracks, releases, playlists, charts, labels, and artists. Add `@beatsource` to a search when you want Beatsource results instead of the default Beatport results.

The original command-line program remains available as `beatportdl` for anyone who prefers terminal use or needs its advanced configuration. Run it once interactively to create `beatportdl-config.yml`, or see the [upstream BeatportDL project](https://github.com/unspok3n/beatportdl) for its complete option and template reference.

## Build and test

```shell
go test ./cmd/beatsweb ./internal/beatport ./config
docker compose build
```

The Docker build runs the Go test suite before producing the final image.

## Project roots and license

Beats is built on [unspok3n/beatportdl](https://github.com/unspok3n/beatportdl). That project supplied the downloader engine; this repository adds the browser workflow, library, conversion pipeline, archive controls, and Docker packaging.

The code is distributed under the [GNU General Public License v3.0](LICENSE). If you redistribute a modified version, keep the license and corresponding source obligations intact.

## Contributing

Found a broken download state, a tag that does not survive conversion, or a library view that could be clearer? Open an issue with the behavior you saw, the source format, and the relevant application log. Pull requests are welcome too.

If Beats fits your setup, star the repository so other Beatport users can find it—and tell us what would make you keep using it.
