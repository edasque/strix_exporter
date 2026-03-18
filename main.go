package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const listenAddr = ":9101"

var (
	gpuPowerAverage = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "amdgpu_power_average_watts",
		Help: "GPU average power draw in watts (from power1_average).",
	})
	gpuPowerInput = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "amdgpu_power_input_watts",
		Help: "GPU input power draw in watts (from power1_input).",
	})
	gpuTempDesc = prometheus.NewDesc(
		"amdgpu_temperature_celsius",
		"GPU temperature in degrees Celsius.",
		[]string{"sensor"}, nil,
	)
	gpuMemDesc = prometheus.NewDesc(
		"amdgpu_memory_bytes",
		"GPU memory in bytes.",
		[]string{"type", "state"}, nil,
	)
	gpuBusyDesc = prometheus.NewDesc(
		"amdgpu_gpu_busy_percent",
		"GPU busy percentage.",
		nil, nil,
	)
	gpuFanDesc = prometheus.NewDesc(
		"amdgpu_fan_rpm",
		"GPU fan speed in RPM.",
		[]string{"fan"}, nil,
	)
	gpuClkDesc = prometheus.NewDesc(
		"amdgpu_clock_mhz",
		"GPU clock frequency in MHz.",
		[]string{"clock"}, nil,
	)
	grbmDesc = prometheus.NewDesc(
		"amdgpu_grbm_busy_percent",
		"GPU engine utilization from GRBM/GRBM2 performance counters.",
		[]string{"register", "engine"}, nil,
	)
)

// findAMDGPUHwmonPath finds the hwmon directory whose "name" file contains "amdgpu".
func findAMDGPUHwmonPath() (string, error) {
	entries, err := os.ReadDir("/sys/class/hwmon")
	if err != nil {
		return "", fmt.Errorf("reading /sys/class/hwmon: %w", err)
	}
	for _, e := range entries {
		p := filepath.Join("/sys/class/hwmon", e.Name())
		nameBytes, err := os.ReadFile(filepath.Join(p, "name"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(nameBytes)) == "amdgpu" {
			return p, nil
		}
	}
	return "", fmt.Errorf("no amdgpu hwmon device found")
}

// readSysfsFloat reads a sysfs file and returns the numeric value.
func readSysfsFloat(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
}

// readSysfsLabel reads a sysfs label file and returns the trimmed string.
func readSysfsLabel(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readGPUMetricsFCLK reads average_fclk_frequency from the gpu_metrics v3.0 binary blob.
// The value is a u16 at byte offset 182 (MHz).
func readGPUMetricsFCLK(drmDevice string) (uint16, error) {
	data, err := os.ReadFile(filepath.Join(drmDevice, "gpu_metrics"))
	if err != nil {
		return 0, err
	}
	// Verify this is format_revision=3 (v3.x)
	if len(data) < 184 || data[2] != 3 {
		return 0, fmt.Errorf("unsupported gpu_metrics format (revision %d, need 3)", data[2])
	}
	return binary.LittleEndian.Uint16(data[182:184]), nil
}

// findDRMDevicePath resolves the hwmon's parent device and finds the matching drm card.
func findDRMDevicePath(hwmonPath string) (string, error) {
	devicePath, err := filepath.EvalSymlinks(filepath.Join(hwmonPath, "device"))
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		name := e.Name()
		// Only match "cardN" entries, not "cardN-DP-1" etc.
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}
		cardDevice := filepath.Join("/sys/class/drm", name, "device")
		resolved, err := filepath.EvalSymlinks(cardDevice)
		if err != nil {
			continue
		}
		if resolved == devicePath {
			return cardDevice, nil
		}
	}
	return "", fmt.Errorf("no drm card found for device %s", devicePath)
}

// findRenderNode finds the /dev/dri/renderDN node for the same PCI device as the hwmon.
func findRenderNode(hwmonPath string) (string, error) {
	devicePath, err := filepath.EvalSymlinks(filepath.Join(hwmonPath, "device"))
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "renderD") {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join("/sys/class/drm", name, "device"))
		if err != nil {
			continue
		}
		if resolved == devicePath {
			return filepath.Join("/dev/dri", name), nil
		}
	}
	return "", fmt.Errorf("no render node found for device %s", devicePath)
}

type amdgpuCollector struct {
	hwmonPath   string
	drmDevice   string
	drmFd       int
	grbmSampler *GRBMSampler
}

func (c *amdgpuCollector) Describe(ch chan<- *prometheus.Desc) {
	gpuPowerAverage.Describe(ch)
	gpuPowerInput.Describe(ch)
	ch <- gpuTempDesc
	ch <- gpuMemDesc
	ch <- gpuBusyDesc
	ch <- gpuClkDesc
	ch <- gpuFanDesc
	ch <- grbmDesc
}

func (c *amdgpuCollector) Collect(ch chan<- prometheus.Metric) {
	// Power metrics
	if v, err := readSysfsFloat(filepath.Join(c.hwmonPath, "power1_average")); err == nil {
		gpuPowerAverage.Set(v / 1_000_000)
	}
	if v, err := readSysfsFloat(filepath.Join(c.hwmonPath, "power1_input")); err == nil {
		gpuPowerInput.Set(v / 1_000_000)
	}
	gpuPowerAverage.Collect(ch)
	gpuPowerInput.Collect(ch)

	// Temperature metrics — discover all tempN_input files
	for i := 1; ; i++ {
		inputPath := filepath.Join(c.hwmonPath, fmt.Sprintf("temp%d_input", i))
		v, err := readSysfsFloat(inputPath)
		if err != nil {
			break
		}
		label := readSysfsLabel(filepath.Join(c.hwmonPath, fmt.Sprintf("temp%d_label", i)))
		if label == "" {
			label = fmt.Sprintf("temp%d", i)
		}
		ch <- prometheus.MustNewConstMetric(gpuTempDesc, prometheus.GaugeValue, v/1000, label)
	}

	// GPU busy percent
	if v, err := readSysfsFloat(filepath.Join(c.drmDevice, "gpu_busy_percent")); err == nil {
		ch <- prometheus.MustNewConstMetric(gpuBusyDesc, prometheus.GaugeValue, v)
	}

	// Clock frequencies via sensor ioctl
	if c.drmFd >= 0 {
		if v, err := readSensorInfo(c.drmFd, sensorGFXSCLK); err == nil {
			ch <- prometheus.MustNewConstMetric(gpuClkDesc, prometheus.GaugeValue, float64(v), "sclk")
		}
		if v, err := readSensorInfo(c.drmFd, sensorGFXMCLK); err == nil {
			ch <- prometheus.MustNewConstMetric(gpuClkDesc, prometheus.GaugeValue, float64(v), "mclk")
		}
	}
	// FCLK from gpu_metrics
	if v, err := readGPUMetricsFCLK(c.drmDevice); err == nil {
		ch <- prometheus.MustNewConstMetric(gpuClkDesc, prometheus.GaugeValue, float64(v), "fclk")
	}

	// Fan RPM — discover all fanN_input files
	for i := 1; ; i++ {
		inputPath := filepath.Join(c.hwmonPath, fmt.Sprintf("fan%d_input", i))
		v, err := readSysfsFloat(inputPath)
		if err != nil {
			break
		}
		ch <- prometheus.MustNewConstMetric(gpuFanDesc, prometheus.GaugeValue, v, fmt.Sprintf("fan%d", i))
	}

	// GRBM / GRBM2 engine utilization
	if c.grbmSampler != nil {
		for _, u := range c.grbmSampler.Sample() {
			ch <- prometheus.MustNewConstMetric(grbmDesc, prometheus.GaugeValue, u.Percent, u.Register, u.Engine)
		}
	}

	// VRAM and GTT memory metrics
	for _, memType := range []string{"vram", "gtt"} {
		for _, state := range []string{"total", "used"} {
			file := filepath.Join(c.drmDevice, fmt.Sprintf("mem_info_%s_%s", memType, state))
			if v, err := readSysfsFloat(file); err == nil {
				ch <- prometheus.MustNewConstMetric(gpuMemDesc, prometheus.GaugeValue, v, memType, state)
			}
		}
	}
}

var enableGRBM = flag.Bool("grbm", false, "Enable GRBM/GRBM2 engine utilization sampling (disables GFXOFF power saving)")

func main() {
	flag.Parse()

	hwmonPath, err := findAMDGPUHwmonPath()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Using hwmon path: %s", hwmonPath)

	drmDevice, err := findDRMDevicePath(hwmonPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Using DRM device path: %s", drmDevice)

	renderNode, err := findRenderNode(hwmonPath)
	if err != nil {
		log.Fatalf("Could not find render node: %v", err)
	}
	renderFile, err := os.OpenFile(renderNode, os.O_RDWR, 0)
	if err != nil {
		log.Fatalf("Could not open %s: %v", renderNode, err)
	}
	drmFd := int(renderFile.Fd())
	log.Printf("Using render node: %s", renderNode)

	var grbmSampler *GRBMSampler
	if *enableGRBM {
		grbmSampler, err = NewGRBMSampler(renderNode)
		if err != nil {
			log.Fatalf("--grbm: could not init GRBM sampler: %v", err)
		}
		log.Printf("GRBM sampling enabled (note: disables GFXOFF power saving)")
	}

	collector := &amdgpuCollector{hwmonPath: hwmonPath, drmDevice: drmDevice, drmFd: drmFd, grbmSampler: grbmSampler}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector)

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><h1>AMD GPU Exporter</h1><a href="/metrics">Metrics</a></body></html>`))
	})

	log.Printf("Listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
