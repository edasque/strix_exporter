# strix_exporter

A Prometheus exporter for AMD GPU metrics, built in Go. Designed for RDNA 3/3.5 APUs (Strix Point) but should work with other amdgpu-driven hardware.

Listens on port **9101**.

## Usage

```
./strix_exporter [--grbm]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--grbm` | disabled | Enable GRBM/GRBM2 engine utilization sampling. **Warning:** this disables GFXOFF power saving on APUs, increasing idle power draw. Each scrape takes ~100ms of register polling. |

## Metrics

### Power

| Metric | Type | Description | Source |
|--------|------|-------------|--------|
| `amdgpu_power_average_watts` | gauge | Average power draw (W) | hwmon `power1_average` (microwatts / 1M) |
| `amdgpu_power_input_watts` | gauge | Input power draw (W) | hwmon `power1_input` (microwatts / 1M) |

### Temperature

| Metric | Labels | Type | Description | Source |
|--------|--------|------|-------------|--------|
| `amdgpu_temperature_celsius` | `sensor` | gauge | Temperature (C) | hwmon `tempN_input` (millidegrees / 1K) |

Auto-discovers all temperature sensors. Labels come from `tempN_label` (e.g. `edge`, `junction`, `mem`).

### Clocks

| Metric | Labels | Type | Description | Source |
|--------|--------|------|-------------|--------|
| `amdgpu_clock_mhz` | `clock` | gauge | Clock frequency (MHz) | varies by clock domain |

Clock sources:

- **`sclk`** (GFX core clock) -- `AMDGPU_INFO_SENSOR` ioctl with `GFX_SCLK` (0x1), via render node
- **`mclk`** (memory clock) -- `AMDGPU_INFO_SENSOR` ioctl with `GFX_MCLK` (0x2), via render node
- **`fclk`** (fabric clock) -- parsed from `gpu_metrics` sysfs binary blob (v3.0, `average_fclk_frequency` at byte offset 182, u16 LE)

### GPU Activity

| Metric | Type | Description | Source |
|--------|------|-------------|--------|
| `amdgpu_gpu_busy_percent` | gauge | Overall GPU busy % | sysfs `gpu_busy_percent` |

### Memory

| Metric | Labels | Type | Description | Source |
|--------|--------|------|-------------|--------|
| `amdgpu_memory_bytes` | `type`, `state` | gauge | Memory (bytes) | sysfs `mem_info_{type}_{state}` |

- `type`: `vram` or `gtt`
- `state`: `total` or `used`

### Fan

| Metric | Labels | Type | Description | Source |
|--------|--------|------|-------------|--------|
| `amdgpu_fan_rpm` | `fan` | gauge | Fan speed (RPM) | hwmon `fanN_input` |

Auto-discovers all fan sensors. Not present on APUs without discrete fan control.

### GRBM Engine Utilization (opt-in: `--grbm`)

| Metric | Labels | Type | Description | Source |
|--------|--------|------|-------------|--------|
| `amdgpu_grbm_busy_percent` | `register`, `engine` | gauge | Per-engine busy % | GRBM/GRBM2 register sampling |

The Graphics Register Bus Manager (GRBM) status registers expose per-engine activity bits. Each scrape samples the registers 100 times at 1ms intervals via `DRM_IOCTL_AMDGPU_INFO` with `AMDGPU_INFO_READ_MMR_REG`, then reports the percentage of samples where each engine's bit was set.

**GRBM engines** (register offset `0x2004`, GFX10+ bit layout):

| Engine | Bit |
|--------|-----|
| Graphics Pipe | 31 |
| Color Block | 30 |
| Depth Block | 26 |
| Primitive Assembly | 25 |
| Shader Processor Interpolator | 22 |
| Geometry Engine | 21 |
| Shader Export | 20 |
| Texture Pipe | 14 |

**GRBM2 engines** (register offset `0x2002`, GFX10.3+ bit layout):

| Engine | Bit |
|--------|-----|
| CP Graphics | 30 |
| CP Compute | 29 |
| CP Fetcher | 28 |
| Texture Cache per Pipe | 27 |
| RunList Controller | 26 |
| SDMA | 21 |
| Render Backend Memory Interface | 17 |
| Efficiency Arbiter | 16 |
| UTCL2 | 15 |

## Architecture

```
strix_exporter
  main.go    -- Prometheus collector, device discovery, sysfs readers
  grbm.go   -- DRM ioctl plumbing, GRBM/GRBM2 register sampling
  drm.go    -- DRM sensor ioctl for clock frequencies
```

### Device discovery

At startup the exporter:

1. Scans `/sys/class/hwmon/` for an entry with `name == "amdgpu"`
2. Resolves the hwmon's `device` symlink to a PCI device path
3. Finds the matching `/sys/class/drm/cardN/device` (filtering out connector entries like `card1-DP-1`)
4. Finds the matching `/dev/dri/renderDN` render node and opens it for ioctl access

### Data sources

The exporter reads from three interfaces:

- **hwmon sysfs** (`/sys/class/hwmon/hwmonN/`) -- power, temperature, fan RPM. Files contain plain-text integers in sensor-specific units (microwatts, millidegrees, RPM).
- **DRM device sysfs** (`/sys/class/drm/cardN/device/`) -- GPU busy %, memory info, gpu_metrics binary blob. Plain-text integers (bytes, percent) except gpu_metrics which is a packed C struct.
- **DRM ioctls** (via `/dev/dri/renderDN`) -- clock frequencies (`AMDGPU_INFO_SENSOR`), register reads (`AMDGPU_INFO_READ_MMR_REG`). Uses the `DRM_IOCTL_AMDGPU_INFO` ioctl (`DRM_IOW(0x45, struct drm_amdgpu_info)`).

All metrics are read fresh on each Prometheus scrape. No background polling or caching.

## Building

```
go build -o strix_exporter .
```

## Example output

```
amdgpu_power_average_watts 62.025
amdgpu_power_input_watts 62.025
amdgpu_temperature_celsius{sensor="edge"} 49
amdgpu_clock_mhz{clock="sclk"} 2739
amdgpu_clock_mhz{clock="mclk"} 1000
amdgpu_clock_mhz{clock="fclk"} 1379
amdgpu_gpu_busy_percent 100
amdgpu_memory_bytes{state="total",type="vram"} 536870912
amdgpu_memory_bytes{state="used",type="vram"} 346882048
amdgpu_memory_bytes{state="total",type="gtt"} 133143986176
amdgpu_memory_bytes{state="used",type="gtt"} 81367519232
```

With `--grbm`:

```
amdgpu_grbm_busy_percent{engine="Graphics Pipe",register="GRBM"} 100
amdgpu_grbm_busy_percent{engine="Texture Pipe",register="GRBM"} 100
amdgpu_grbm_busy_percent{engine="CP Compute",register="GRBM2"} 100
amdgpu_grbm_busy_percent{engine="CP Graphics",register="GRBM2"} 0
...
```
