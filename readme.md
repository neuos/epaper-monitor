# ePaper Monitor

A self-hosted system that displays live ÖBB (Austrian Federal Railways) departure information on a Waveshare 7.5" e-paper display powered by a Seeed reTerminal E1001 (ESP32-S3).

## Architecture

```
┌─────────────┐       ┌──────────────────┐       ┌──────────────────┐
│  ÖBB Scotty │       │  Oracle Server   │       │  reTerminal E1001│
│  JSONP API  │◄──────│                  │       │  (ESP32-S3)      │
└─────────────┘       │  ┌────────────┐  │  CSV  │                  │
                      │  │oebb-monitor│──┼──────►│  Parse CSV +     │
                      │  │  (Go, :80) │  │  ~1KB │  draw table on   │
                      │  └────────────┘  │       │  7.5" e-paper    │
                      │                  │       │                  │
                      │  Caddy (TLS)     │       │  Deep sleep +    │
                      │                  │       │  hourly wakeup   │
                      └──────────────────┘       └──────────────────┘
```

**Data flow:**

1. The **oebb-monitor** Go service fetches live departure data from the ÖBB Scotty JSONP API for one or more stations, merges and sorts them, and returns a CSV response (~1 KB).
2. The **reTerminal E1001** running ESPHome firmware fetches that CSV over HTTPS, parses it on-device, draws the departure table directly on the e-paper display, and goes to deep sleep.

No screenshot service or headless browser involved — the ESP32 draws the table natively.

## Components

| Component | Path | Runs on | Purpose |
|---|---|---|---|
| [oebb-monitor](oebb-monitor/) | `~/oebb-monitor-v2` on server | Docker (Go binary) | Fetches ÖBB API, returns CSV |
| [esphome config](esphome/) | Local machine | Flashed to ESP32-S3 | ESPHome firmware for the e-paper device |

## Server Setup

The server (`ssh oracle`) runs the oebb-monitor service as a Docker container, with Caddy handling TLS and reverse proxying.

### Caddy (reverse proxy)

Config at `/etc/caddy/Caddyfile`:

```
oebb.oracle.neuhuber.eu {
    reverse_proxy :5010
}
```

Apply changes:

```sh
sudo systemctl reload caddy
```

### oebb-monitor

Stack at `/opt/stacks/oebb-monitor/compose.yaml`, image built from `~/oebb-monitor-v2/`.

```yaml
services:
  oebb-monitor:
    image: oebb-monitor
    container_name: oebb-monitor
    ports:
      - 5010:80
    restart: unless-stopped
```

Build and deploy:

```sh
cd ~/oebb-monitor-v2
docker build -t oebb-monitor .
cd /opt/stacks/oebb-monitor
docker compose up -d --force-recreate
```

### ESPHome firmware

Compiled and flashed locally from `esphome/reterminal-e1001.yaml`:

```sh
esphome run --device /dev/cu.wchusbserial10 config/reterminal-e1001.yaml
```

## oebb-monitor — `/departures.csv`

A single Go binary that fetches departures from one or more ÖBB stations and returns a merged, sorted CSV. Designed to be consumed directly by the ESP32.

### Query Parameters

| Parameter | Default | Description |
|---|---|---|
| `stations` | *(required)* | Comma-separated list of ÖBB station IDs. Optional direction filter(s) per station via colon: `stationId:dir1:dir2` |
| `num_journeys` | `6` | Number of departures to fetch per station |
| `additional_time` | `0` | Lead time in minutes (skip departures sooner than this) |
| `total` | `12` | Maximum rows in the merged result |
| `products_filter` | `1011111111011` | ÖBB product bitmask filter |

### CSV Format

The response is `text/csv; charset=utf-8`. The first row contains the current server time (Europe/Vienna) in the first column, followed by column headers. Data rows follow, sorted by departure time.

```
21:12,Linie,Von,Richtung
21:17,S 3,Matzl Pl.,Floridsdorf Bhf
21:19,Bus 14A,Spengerg.,Neubaug. (Schadekg.)
21:20,S 1,Matzl Pl.,Gänserndorf Bhf
```

| Column | Content |
|---|---|
| **Zeit** | Actual departure time (real-time if delayed, scheduled if on time) |
| **Linie** | Line name (e.g. `REX 1`, `Tram 18`, `S 2`, `Bus B01`) |
| **Von** | Departure station name (shortened) |
| **Richtung** | Direction / terminal station (shortened) |

Cancelled departures (`Ausfall`) are filtered out. Station and direction names are shortened (e.g. "Wien Matzleinsdorfer Platz" → "Matzl Pl."). HTML entities from the API are decoded.

### Example

```
https://oebb.oracle.neuhuber.eu/departures.csv?stations=1290501:1292001,0905026,1390563&num_journeys=10&additional_time=5&total=10
```

### Finding Station IDs

Use the [ÖBB Link Creator](https://dave2ooo.github.io/oebb-link-creator/html/mode1.html) to look up station IDs interactively.

## ESPHome — reTerminal E1001

The ESP32-S3 based device runs ESPHome firmware that fetches the CSV and draws the departure table natively on the e-paper display.

### Display

- **Model:** Waveshare 7.5" e-paper V2 (`7.50inV2p`), 800×480 pixels, black & white
- **SPI pins:** CLK=GPIO7, MOSI=GPIO9, CS=GPIO10, DC=GPIO11, RST=GPIO12, BUSY=GPIO13
- **Fonts:** Roboto Condensed (Google Fonts), downloaded at compile time
- **Table layout:** 4 columns (Zeit, Linie, Von, Richtung) with alternating row backgrounds

### How It Works

1. On wakeup (or button press), the device makes an HTTP GET to the `/departures.csv` endpoint.
2. The CSV response (~1 KB) is stored in a `std::string` global.
3. The display lambda parses the CSV line-by-line, extracts the 4 fields per row, and draws:
   - A clock (top-right, from the CSV header row)
   - Battery icon + temperature (top-left)
   - Column headers with underline
   - Up to 10 data rows with alternating black/white backgrounds
4. After rendering, the device enters deep sleep.

### Navigation

Two physical buttons trigger a data refresh:

- **Right button** (GPIO4): Refresh departures
- **Left button** (GPIO5): Refresh departures

Both buttons reset the sleep timer.

### Sleep Behavior

- After 90 seconds of inactivity, the device checks the time and enters deep sleep.
- **Rush hour (06:50–07:20):** Sleeps for 2 minutes, then wakes to refresh.
- **Daytime (06:00–22:00):** Sleeps for 60 minutes, then wakes to refresh.
- **Nighttime (22:00–06:00):** Sleeps until 6:00 AM.
- **GPIO3 wakeup:** Physical wakeup button refreshes departures.

### Sensors

| Sensor | Description |
|---|---|
| Temperature | SHT4x sensor via I2C |
| Relative Humidity | SHT4x sensor via I2C |
| Battery Voltage | ADC on GPIO1, multiplied by 2.0 |
| Battery Level | Derived from voltage, 0–100% via calibration curve |