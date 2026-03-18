package main

import (
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Register offsets (dword offsets, as used by the kernel).
const (
	grbmOffset  = 0x2004 // GRBM_STATUS
	grbm2Offset = 0x2002 // GRBM2_STATUS2
)

// AMDGPU DRM ioctl constants.
const (
	drmCommandBase       = 0x40
	drmAmdgpuInfoCmd         = 0x05
	amdgpuInfoReadMMRReg = 0x15
)

// drm_amdgpu_info for READ_MMR_REG query.
// Must match the kernel struct layout exactly.
type drmAmdgpuInfoQuery struct {
	ReturnPointer uint64
	ReturnSize    uint32
	Query         uint32
	// Union: read_mmr_reg variant
	DwordOffset uint32
	Count       uint32
	Instance    uint32
	Flags       uint32
}

func drmIoctlAmdgpuInfo() uintptr {
	// DRM_IOW(DRM_COMMAND_BASE + DRM_AMDGPU_INFO, struct drm_amdgpu_info)
	// IOW = _IOC(_IOC_WRITE, 'd', nr, size)
	// _IOC_WRITE = 1, type = 'd' = 0x64
	const iocWrite = 1
	const iocTypeBits = 8
	const iocNrBits = 8
	const iocSizeBits = 14
	const iocNrShift = 0
	const iocTypeShift = iocNrShift + iocNrBits   // 8
	const iocSizeShift = iocTypeShift + iocTypeBits // 16
	const iocDirShift = iocSizeShift + iocSizeBits  // 30

	nr := uint32(drmCommandBase + drmAmdgpuInfoCmd) // 0x45
	size := uint32(unsafe.Sizeof(drmAmdgpuInfoQuery{}))

	return uintptr(iocWrite<<iocDirShift | uint32('d')<<iocTypeShift | nr<<iocNrShift | size<<iocSizeShift)
}

func readMMRReg(fd int, dwordOffset uint32) (uint32, error) {
	var value uint32

	info := drmAmdgpuInfoQuery{
		ReturnPointer: uint64(uintptr(unsafe.Pointer(&value))),
		ReturnSize:    4,
		Query:         amdgpuInfoReadMMRReg,
		DwordOffset:   dwordOffset,
		Count:         1,
		Instance:      0xFFFFFFFF,
		Flags:         0,
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(fd),
		drmIoctlAmdgpuInfo(),
		uintptr(unsafe.Pointer(&info)),
	)
	if errno != 0 {
		return 0, fmt.Errorf("ioctl READ_MMR_REG offset 0x%x: %w", dwordOffset, errno)
	}
	return value, nil
}

// grbmEngine defines a named bit position in a GRBM/GRBM2 status register.
type grbmEngine struct {
	Name string
	Bit  uint
}

// GFX10 GRBM engines (used for RDNA 2/3/3.5 including Strix).
var grbmEngines = []grbmEngine{
	{"Graphics Pipe", 31},
	{"Texture Pipe", 14},
	{"Shader Export", 20},
	{"Shader Processor Interpolator", 22},
	{"Primitive Assembly", 25},
	{"Depth Block", 26},
	{"Color Block", 30},
	{"Geometry Engine", 21},
}

// GFX10.3+ GRBM2 engines (used for RDNA 2/3/3.5 including Strix).
var grbm2Engines = []grbmEngine{
	{"RunList Controller", 26},
	{"Texture Cache per Pipe", 27},
	{"UTCL2", 15},
	{"Efficiency Arbiter", 16},
	{"Render Backend Memory Interface", 17},
	{"SDMA", 21},
	{"CP Fetcher", 28},
	{"CP Compute", 29},
	{"CP Graphics", 30},
}

const grbmSamples = 100

// GRBMSampler holds an open file descriptor to the render node.
type GRBMSampler struct {
	fd int
}

// NewGRBMSampler opens the render node for register reads.
func NewGRBMSampler(renderNode string) (*GRBMSampler, error) {
	fd, err := os.OpenFile(renderNode, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", renderNode, err)
	}
	// Test that we can actually read a register.
	if _, err := readMMRReg(int(fd.Fd()), grbmOffset); err != nil {
		fd.Close()
		return nil, fmt.Errorf("test read GRBM: %w", err)
	}
	return &GRBMSampler{fd: int(fd.Fd())}, nil
}

// GRBMUsage holds per-engine utilization percentages (0-100).
type GRBMUsage struct {
	Register string // "GRBM" or "GRBM2"
	Engine   string
	Percent  float64
}

// Sample reads GRBM and GRBM2 registers repeatedly and returns per-engine usage.
func (s *GRBMSampler) Sample() []GRBMUsage {
	grbmCounts := make([]int, len(grbmEngines))
	grbm2Counts := make([]int, len(grbm2Engines))

	for i := 0; i < grbmSamples; i++ {
		if val, err := readMMRReg(s.fd, grbmOffset); err == nil {
			for j, eng := range grbmEngines {
				if val&(1<<eng.Bit) != 0 {
					grbmCounts[j]++
				}
			}
		}
		if val, err := readMMRReg(s.fd, grbm2Offset); err == nil {
			for j, eng := range grbm2Engines {
				if val&(1<<eng.Bit) != 0 {
					grbm2Counts[j]++
				}
			}
		}
		time.Sleep(1 * time.Millisecond)
	}

	var results []GRBMUsage
	for j, eng := range grbmEngines {
		results = append(results, GRBMUsage{
			Register: "GRBM",
			Engine:   eng.Name,
			Percent:  float64(grbmCounts[j]) / float64(grbmSamples) * 100,
		})
	}
	for j, eng := range grbm2Engines {
		results = append(results, GRBMUsage{
			Register: "GRBM2",
			Engine:   eng.Name,
			Percent:  float64(grbm2Counts[j]) / float64(grbmSamples) * 100,
		})
	}
	return results
}
