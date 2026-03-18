package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// AMDGPU sensor types.
const (
	amdgpuInfoSensor        = 0x1D
	sensorGFXSCLK           = 0x1 // MHz
	sensorGFXMCLK           = 0x2 // MHz
)

// readSensorInfo queries AMDGPU_INFO_SENSOR for the given sensor type.
// Returns the value as a uint32.
func readSensorInfo(fd int, sensorType uint32) (uint32, error) {
	var value uint32

	info := drmAmdgpuInfoQuery{
		ReturnPointer: uint64(uintptr(unsafe.Pointer(&value))),
		ReturnSize:    4,
		Query:         amdgpuInfoSensor,
		DwordOffset:   sensorType, // sensor_info.type occupies same union position
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(fd),
		drmIoctlAmdgpuInfo(),
		uintptr(unsafe.Pointer(&info)),
	)
	if errno != 0 {
		return 0, fmt.Errorf("ioctl SENSOR type 0x%x: %w", sensorType, errno)
	}
	return value, nil
}
